package manager

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wongpinter/gdm/internal/domain"
)

// ErrNotFound is returned by any operation on an unknown download ID.
var ErrNotFound = errors.New("download not found")

// Snapshot pairs a point-in-time copy of a Download with runtime-only
// data (speed, peers) that doesn't belong in the persisted domain entity.
type Snapshot struct {
	Download *domain.Download
	SpeedBps float64
	Peers    int
}

// entry is the manager's private, mutable record for one download.
// dl is guarded by mu; cancel is non-nil exactly while a worker
// goroutine is running this download's segments.
type entry struct {
	mu     sync.Mutex
	dl     *domain.Download
	cancel context.CancelFunc
	runSeq uint64
	runID  uint64

	lastSample time.Time
	lastBytes  int64
	speed      float64
	peers      int
}

// Manager is the application service at the center of the hexagon: it
// depends only on the Engine/TorrentEngine and Store ports, never on a
// concrete transport or storage format. torrentEngine may be nil — a
// Manager works fine as HTTP-only until SetTorrentEngine is called.
type Manager struct {
	engine        Engine
	torrentEngine TorrentEngine
	store         Store

	downloadDir string
	maxConns    int
	maxActive   int

	mu      sync.RWMutex
	entries map[string]*entry
	order   []string

	sem    chan struct{}
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
}

// New builds a Manager, creates downloadDir if needed, and reloads any
// downloads the store already knows about. Downloads that were mid-flight
// when the process last exited are requeued as Paused rather than
// silently resumed, so the user decides when to spend bandwidth again.
func New(eng Engine, st Store, downloadDir string, maxConns, maxActive int) (*Manager, error) {
	if maxConns <= 0 {
		maxConns = 4
	}
	if maxActive <= 0 {
		maxActive = 3
	}
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating download dir: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		engine:      eng,
		store:       st,
		downloadDir: downloadDir,
		maxConns:    maxConns,
		maxActive:   maxActive,
		entries:     make(map[string]*entry),
		sem:         make(chan struct{}, maxActive),
		ctx:         ctx,
		cancel:      cancel,
	}

	saved, err := st.LoadAll()
	if err != nil {
		cancel()
		return nil, err
	}
	slices.SortFunc(saved, func(a, b *domain.Download) int { return a.CreatedAt.Compare(b.CreatedAt) })
	for _, dl := range saved {
		if dl.Status == domain.StatusDownloading || dl.Status == domain.StatusProbing {
			dl.Status = domain.StatusPaused
		}
		m.entries[dl.ID] = &entry{dl: dl}
		m.order = append(m.order, dl.ID)
		if dl.Status == domain.StatusQueued {
			m.enqueue(dl.ID)
		}
	}
	return m, nil
}

// SetTorrentEngine wires up torrent support. Calling AddTorrent before
// this is set fails with a clear error rather than a nil dereference.
func (m *Manager) SetTorrentEngine(te TorrentEngine) {
	m.torrentEngine = te
}

// Add registers a new HTTP download and schedules it for probing.
// connections <= 0 falls back to the manager's default.
func (m *Manager) Add(rawURL string, connections int) (*domain.Download, error) {
	return m.AddAt(rawURL, connections, time.Time{})
}

// AddAt registers an HTTP download and waits until startAt before probing.
// A zero startAt starts immediately; a past startAt also starts immediately.
func (m *Manager) AddAt(rawURL string, connections int, startAt time.Time) (*domain.Download, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, errors.New("URL is empty")
	}
	if connections <= 0 {
		connections = m.maxConns
	}

	now := time.Now()
	dl := &domain.Download{
		ID:          newID(),
		URL:         rawURL,
		Kind:        domain.KindHTTP,
		Connections: connections,
		Status:      domain.StatusQueued,
		StartAt:     startAt,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	return m.add(dl)
}

// AddTorrent registers a new torrent download from a magnet URI or a
// path to a local .torrent file.
func (m *Manager) AddTorrent(uri string) (*domain.Download, error) {
	return m.AddTorrentAt(uri, time.Time{})
}

// AddTorrentAt registers a torrent and waits until startAt before joining.
// A zero startAt starts immediately; a past startAt also starts immediately.
func (m *Manager) AddTorrentAt(uri string, startAt time.Time) (*domain.Download, error) {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return nil, errors.New("magnet URI or .torrent path is empty")
	}
	now := time.Now()
	dl := &domain.Download{
		ID:        newID(),
		URL:       uri,
		Kind:      domain.KindTorrent,
		Status:    domain.StatusQueued,
		StartAt:   startAt,
		CreatedAt: now,
		UpdatedAt: now,
	}
	return m.add(dl)
}

func (m *Manager) add(dl *domain.Download) (*domain.Download, error) {
	m.mu.Lock()
	m.entries[dl.ID] = &entry{dl: dl}
	m.order = append(m.order, dl.ID)
	m.mu.Unlock()

	if err := m.store.Save(dl); err != nil {
		m.mu.Lock()
		delete(m.entries, dl.ID)
		m.order = slices.DeleteFunc(m.order, func(x string) bool { return x == dl.ID })
		m.mu.Unlock()
		return nil, err
	}
	// Clone before enqueueing — the worker goroutine starts mutating
	// dl immediately, so the caller must receive an independent copy.
	snap := dl.Clone()
	m.enqueue(dl.ID)
	return snap, nil
}

// Pause cancels an actively downloading (or probing) transfer. The
// worker goroutine notices ctx cancellation, persists progress so far,
// and marks the download Paused.
func (m *Manager) Pause(id string) error {
	e := m.getEntry(id)
	if e == nil {
		return ErrNotFound
	}
	e.mu.Lock()
	cancel := e.cancel
	e.mu.Unlock()
	if cancel == nil {
		return fmt.Errorf("%s is not active", id)
	}
	cancel()
	return nil
}

// Resume requeues a paused or failed download.
func (m *Manager) Resume(id string) error {
	e := m.getEntry(id)
	if e == nil {
		return ErrNotFound
	}
	e.mu.Lock()
	if e.dl.Active() {
		e.mu.Unlock()
		return fmt.Errorf("%s is already active", id)
	}
	e.dl.Status = domain.StatusQueued
	e.dl.Error = ""
	dl := e.dl.Clone()
	e.mu.Unlock()

	if err := m.store.Save(dl); err != nil {
		return err
	}
	m.enqueue(id)
	return nil
}

// Remove cancels the download if active, drops the torrent from the
// swarm (if applicable), forgets it, and optionally deletes the partial
// (or complete) file/directory it wrote to disk.
func (m *Manager) Remove(id string, deleteFile bool) error {
	e := m.getEntry(id)
	if e == nil {
		return ErrNotFound
	}
	e.mu.Lock()
	cancel := e.cancel
	dest := e.dl.Dest
	kind := e.dl.EffectiveKind()
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	// Drop the torrent from the swarm — this is the only code path
	// that does it. Pause keeps the torrent in the client.
	if kind == domain.KindTorrent && m.torrentEngine != nil {
		m.torrentEngine.Remove(id)
	}

	m.mu.Lock()
	delete(m.entries, id)
	m.order = slices.DeleteFunc(m.order, func(x string) bool { return x == id })
	m.mu.Unlock()

	if err := m.store.Delete(id); err != nil {
		return err
	}
	if deleteFile && dest != "" {
		if kind == domain.KindTorrent {
			_ = os.RemoveAll(dest)
		} else {
			_ = os.Remove(dest)
		}
	}
	return nil
}

// Get returns a point-in-time snapshot of one download.
func (m *Manager) Get(id string) (Snapshot, bool) {
	e := m.getEntry(id)
	if e == nil {
		return Snapshot{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return Snapshot{Download: e.dl.Clone(), SpeedBps: e.speed, Peers: e.peers}, true
}

// List returns every known download in the order it was added.
func (m *Manager) List() []Snapshot {
	m.mu.RLock()
	ids := append([]string(nil), m.order...)
	m.mu.RUnlock()

	out := make([]Snapshot, 0, len(ids))
	for _, id := range ids {
		if snap, ok := m.Get(id); ok {
			out = append(out, snap)
		}
	}
	return out
}

// Shutdown cancels every in-flight download and waits (up to timeout)
// for their goroutines to persist final state and exit.
func (m *Manager) Shutdown(timeout time.Duration) {
	m.cancel()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (m *Manager) getEntry(id string) *entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.entries[id]
}

func (m *Manager) enqueue(id string) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if e := m.getEntry(id); e != nil {
			e.mu.Lock()
			startAt := e.dl.StartAt
			e.mu.Unlock()
			if !startAt.IsZero() && time.Until(startAt) > 0 {
				timer := time.NewTimer(time.Until(startAt))
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-m.ctx.Done():
					return
				}
			}
		}
		select {
		case m.sem <- struct{}{}:
		case <-m.ctx.Done():
			return
		}
		defer func() { <-m.sem }()

		e := m.getEntry(id)
		if e == nil {
			return
		}
		e.mu.Lock()
		kind := e.dl.EffectiveKind()
		e.mu.Unlock()

		if kind == domain.KindTorrent {
			m.runTorrentDownload(id)
		} else {
			m.runDownload(id)
		}
	}()
}

// runDownload drives one download from Queued through to a terminal
// (or Paused) state: probe if needed, split into segments, hand off to
// the engine, and aggregate the ProgressEvents it emits.
func (m *Manager) runDownload(id string) {
	e := m.getEntry(id)
	if e == nil {
		return
	}

	ctx, cancel := context.WithCancel(m.ctx)
	e.mu.Lock()
	e.runSeq++
	runID := e.runSeq
	e.runID = runID
	e.cancel = cancel
	needsProbe := len(e.dl.Segments) == 0
	e.mu.Unlock()

	if needsProbe {
		e.setStatus(domain.StatusProbing)
		m.persist(id)

		res, err := m.engine.Probe(ctx, e.dl.URL)
		if err != nil {
			m.finish(id, runID, err, ctx)
			e.clearCancel(runID)
			return
		}

		e.mu.Lock()
		if e.dl.Filename == "" {
			e.dl.Filename = res.Filename
		}
		if e.dl.Dest == "" {
			e.dl.Dest = filepath.Join(m.downloadDir, uniqueName(m.downloadDir, e.dl.Filename))
		}
		e.dl.TotalSize = res.Size
		e.dl.SupportsRange = res.SupportsRange
		e.dl.Segments = splitSegments(res.Size, e.dl.Connections, res.SupportsRange)
		e.mu.Unlock()
	}

	e.setStatus(domain.StatusDownloading)
	m.persist(id)

	e.mu.Lock()
	runInput := e.dl.Clone()
	e.mu.Unlock()

	events := make(chan ProgressEvent, 64)
	done := make(chan error, 1)
	go func() { done <- m.engine.Run(ctx, runInput, events) }()

	speedTicker := time.NewTicker(750 * time.Millisecond)
	defer speedTicker.Stop()
	persistTicker := time.NewTicker(2 * time.Second)
	defer persistTicker.Stop()

	for {
		select {
		case ev := <-events:
			e.applyEvent(ev)
		case <-speedTicker.C:
			e.updateSpeed()
		case <-persistTicker.C:
			m.persist(id)
		case err := <-done:
			drainEvents(events, e)
			m.finish(id, runID, err, ctx)
			e.clearCancel(runID)
			return
		}
	}
}

func drainEvents(events chan ProgressEvent, e *entry) {
	for {
		select {
		case ev := <-events:
			e.applyEvent(ev)
		default:
			return
		}
	}
}

// runTorrentDownload drives one torrent download from Queued through to
// completion. Unlike runDownload it has no probe phase — the torrent
// library handles metadata & piece selection. The engine keeps the
// torrent in the client across pause/resume, so a resume picks up
// instantly without re-adding or re-verifying.
func (m *Manager) runTorrentDownload(id string) {
	e := m.getEntry(id)
	if e == nil {
		return
	}
	if m.torrentEngine == nil {
		e.fail(errors.New("torrent support is not configured"))
		m.persist(id)
		return
	}

	ctx, cancel := context.WithCancel(m.ctx)
	e.mu.Lock()
	e.runSeq++
	runID := e.runSeq
	e.runID = runID
	e.cancel = cancel
	e.mu.Unlock()

	e.setStatus(domain.StatusDownloading)
	m.persist(id)

	e.mu.Lock()
	runInput := e.dl.Clone()
	e.mu.Unlock()

	stats := make(chan TorrentStats, 16)
	done := make(chan error, 1)
	go func() { done <- m.torrentEngine.Start(ctx, id, runInput, stats) }()

	// Piece events come in ~every 250ms from the engine — use them as
	// speed-update triggers so we don't need a separate ticker.
	persistTicker := time.NewTicker(2 * time.Second)
	defer persistTicker.Stop()

	for {
		select {
		case st := <-stats:
			m.applyTorrentStats(e, st)
			e.updateSpeed()
		case <-persistTicker.C:
			m.persist(id)
		case err := <-done:
			drainTorrentStats(stats, m, e)
			m.finish(id, runID, err, ctx)
			e.clearCancel(runID)
			return
		}
	}
}

func drainTorrentStats(stats chan TorrentStats, m *Manager, e *entry) {
	for {
		select {
		case st := <-stats:
			m.applyTorrentStats(e, st)
		default:
			return
		}
	}
}

// applyTorrentStats folds a TorrentStats snapshot into the entry. The
// whole torrent is represented as a single pseudo-Segment so
// Download.BytesDownloaded/Progress work unchanged for both kinds.
func (m *Manager) applyTorrentStats(e *entry, st TorrentStats) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dl.Filename == "" && st.Name != "" {
		e.dl.Filename = st.Name
	}
	if e.dl.Dest == "" && st.Name != "" {
		e.dl.Dest = filepath.Join(m.downloadDir, st.Name)
	}
	if st.TotalSize > 0 {
		e.dl.TotalSize = st.TotalSize
		e.dl.Segments = []domain.Segment{{Index: 0, Start: 0, End: st.TotalSize - 1, Downloaded: st.BytesDownloaded}}
	}
	e.peers = st.Peers
	e.dl.UpdatedAt = time.Now()
}

func (m *Manager) finish(id string, runID uint64, runErr error, ctx context.Context) {
	e := m.getEntry(id)
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.runID != runID {
		e.mu.Unlock()
		return
	}
	switch {
	case runErr == nil:
		e.dl.Status = domain.StatusCompleted
		e.dl.Error = ""
		if e.dl.TotalSize <= 0 {
			e.dl.TotalSize = e.dl.BytesDownloaded()
		}
	case errors.Is(runErr, context.Canceled) && ctx.Err() != nil:
		e.dl.Status = domain.StatusPaused
	default:
		e.dl.Status = domain.StatusFailed
		e.dl.Error = runErr.Error()
	}
	e.dl.UpdatedAt = time.Now()
	e.mu.Unlock()
	m.persist(id)
}

func (m *Manager) persist(id string) {
	e := m.getEntry(id)
	if e == nil {
		return
	}
	e.mu.Lock()
	dl := e.dl.Clone()
	e.mu.Unlock()
	_ = m.store.Save(dl)
}

func (e *entry) setStatus(s domain.Status) {
	e.mu.Lock()
	e.dl.Status = s
	e.dl.UpdatedAt = time.Now()
	e.mu.Unlock()
}

func (e *entry) fail(err error) {
	e.mu.Lock()
	e.dl.Status = domain.StatusFailed
	e.dl.Error = err.Error()
	e.dl.UpdatedAt = time.Now()
	e.mu.Unlock()
}

func (e *entry) clearCancel(runID uint64) {
	e.mu.Lock()
	if e.runID == runID {
		e.cancel = nil
	}
	e.mu.Unlock()
}

func (e *entry) applyEvent(ev ProgressEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.dl.Segments {
		if e.dl.Segments[i].Index == ev.SegmentIndex {
			e.dl.Segments[i].Downloaded += ev.BytesWritten
			break
		}
	}
	e.dl.UpdatedAt = time.Now()
}

func (e *entry) updateSpeed() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	current := e.dl.BytesDownloaded()
	if e.lastSample.IsZero() {
		e.lastSample = now
		e.lastBytes = current
		return
	}
	elapsed := now.Sub(e.lastSample).Seconds()
	if elapsed <= 0 {
		return
	}
	instant := float64(current-e.lastBytes) / elapsed
	const smoothing = 0.3
	e.speed = smoothing*instant + (1-smoothing)*e.speed
	e.lastBytes = current
	e.lastSample = now
}

// splitSegments divides size bytes across connections. When size is
// unknown or the server rejects ranges, it falls back to a single
// unbounded segment (End = -1 means "read until EOF").
func splitSegments(size int64, connections int, supportsRange bool) []domain.Segment {
	if !supportsRange || size <= 0 {
		return []domain.Segment{{Index: 0, Start: 0, End: -1}}
	}
	if connections < 1 {
		connections = 1
	}
	if int64(connections) > size {
		connections = int(size)
	}
	chunk := size / int64(connections)
	segments := make([]domain.Segment, 0, connections)
	start := int64(0)
	for i := range connections {
		end := start + chunk - 1
		if i == connections-1 {
			end = size - 1
		}
		segments = append(segments, domain.Segment{Index: i, Start: start, End: end})
		start = end + 1
	}
	return segments
}

// uniqueName appends " (n)" before the extension until it finds a name
// that doesn't already exist in dir, so a second download of the same
// file never clobbers the first.
func uniqueName(dir, name string) string {
	if name == "" {
		name = "download"
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	candidate := name
	for i := 1; ; i++ {
		if _, err := os.Stat(filepath.Join(dir, candidate)); os.IsNotExist(err) {
			return candidate
		}
		candidate = fmt.Sprintf("%s (%d)%s", base, i, ext)
	}
}

func newID() string {
	return fmt.Sprintf("%d-%04x", time.Now().UnixNano(), rand.IntN(0x10000))
}
