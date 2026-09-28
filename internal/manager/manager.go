package manager

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wongpinter/gdm/internal/domain"
	"github.com/wongpinter/gdm/internal/magnet"
)

// ErrNotFound is returned by any operation on an unknown download ID.
var ErrNotFound = errors.New("download not found")

// Snapshot pairs a point-in-time copy of a Download with runtime-only
// data (speed, peers, torrent stage) that doesn't belong in the
// persisted domain entity.
type Snapshot struct {
	Download *domain.Download
	SpeedBps float64
	Peers    int
	// TorrentStage/TorrentStalled mirror the engine's phase for
	// KindTorrent; empty/false for HTTP downloads.
	TorrentStage   string
	TorrentStalled bool
	TorrentFound   int
	TorrentPending int
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
	// speedBytes is a transport-provided monotonic byte counter. HTTP
	// leaves it at zero and uses Download.BytesDownloaded; torrents use
	// cumulative useful network bytes so hash failures don't yield
	// negative or erratic speed readings.
	speedBytes int64
	speed      float64
	peers      int
	// torrentStage/torrentStalled mirror the latest TorrentStats phase;
	// surfaced via Snapshot for TUI/CLI display.
	torrentStage   string
	torrentStalled bool
	torrentFound   int
	torrentPending int
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
		// A torrent marked completed while its files never left .part
		// isn't finished: anacrolix logs promotion failures and moves
		// on, so the piece state lied. Demote to Paused so the user
		// can resume and let the engine's repair pass rehash and
		// promote them. Re-derived on every load, so it heals itself
		// once the .part files are gone.
		if dl.Status == domain.StatusCompleted && dl.EffectiveKind() == domain.KindTorrent &&
			dl.Dest != "" && partDataRemains(dl.Dest) {
			dl.Status = domain.StatusPaused
			dl.Error = "files not finalized (.part remains); resume to repair"
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
// A URI matching a known torrent by exact URL or info hash returns the
// existing download (resumed when terminal) instead of queueing a
// duplicate — tracker lists and display names don't change identity.
func (m *Manager) AddTorrentAt(uri string, startAt time.Time) (*domain.Download, error) {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return nil, errors.New("magnet URI or .torrent path is empty")
	}
	ih, _ := magnet.InfoHashOf(uri)
	if e := m.findTorrent(uri, ih); e != nil {
		e.mu.Lock()
		status := e.dl.Status
		id := e.dl.ID
		e.mu.Unlock()
		switch status {
		case domain.StatusFailed, domain.StatusCanceled, domain.StatusPaused:
			if err := m.Resume(id); err != nil {
				return nil, err
			}
		}
		if snap, ok := m.Get(id); ok {
			return snap.Download, nil
		}
	}
	now := time.Now()
	dl := &domain.Download{
		ID:        newID(),
		URL:       uri,
		Kind:      domain.KindTorrent,
		InfoHash:  ih,
		Status:    domain.StatusQueued,
		StartAt:   startAt,
		CreatedAt: now,
		UpdatedAt: now,
	}
	return m.add(dl)
}

// findTorrent returns the entry for a known torrent matching uri
// exactly or sharing a non-empty info hash. Callers must not hold m.mu.
func (m *Manager) findTorrent(uri, ih string) *entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, id := range m.order {
		e, ok := m.entries[id]
		if !ok {
			continue
		}
		e.mu.Lock()
		match := e.dl.EffectiveKind() == domain.KindTorrent &&
			(e.dl.URL == uri || (ih != "" && e.dl.InfoHash == ih))
		e.mu.Unlock()
		if match {
			return e
		}
	}
	return nil
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

// Pause cancels an actively downloading (or probing) transfer, or
// holds back a queued one whose worker hasn't claimed it yet — either
// way the download ends up Paused. An active transfer notices ctx
// cancellation and persists its progress; a queued one is flipped to
// Paused directly so beginRun refuses to start it.
func (m *Manager) Pause(id string) error {
	e := m.getEntry(id)
	if e == nil {
		return ErrNotFound
	}
	e.mu.Lock()
	cancel := e.cancel
	if cancel != nil {
		e.mu.Unlock()
		cancel()
		return nil
	}
	if e.dl.Status != domain.StatusQueued {
		e.mu.Unlock()
		return fmt.Errorf("%s is not active", id)
	}
	e.dl.Status = domain.StatusPaused
	e.dl.UpdatedAt = time.Now()
	e.mu.Unlock()
	m.persist(id)
	return nil
}

// Resume requeues a paused or failed download. The in-memory Queued
// state is rolled back if persisting it fails, so memory never claims
// a state the store rejected and no worker was scheduled for.
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
	prevStatus, prevErr := e.dl.Status, e.dl.Error
	e.dl.Status = domain.StatusQueued
	e.dl.Error = ""
	e.dl.UpdatedAt = time.Now()
	// Invalidate any still-lingering previous run so its late finish
	// can't flip the Queued state set here back to Paused.
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
	e.runSeq++
	e.runID = e.runSeq
	dl := e.dl.Clone()
	e.mu.Unlock()

	if err := m.store.Save(dl); err != nil {
		e.mu.Lock()
		if e.dl.Status == domain.StatusQueued { // untouched while saving
			e.dl.Status = prevStatus
			e.dl.Error = prevErr
		}
		e.mu.Unlock()
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
	// dest is expected to live inside downloadDir; the check is belt
	// and braces for a state file written by an older or hostile build,
	// because RemoveAll on an escaping path deletes user data outside
	// the download directory.
	if deleteFile && dest != "" && withinDir(m.downloadDir, dest) {
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
	return Snapshot{Download: e.dl.Clone(), SpeedBps: e.speed, Peers: e.peers, TorrentStage: e.torrentStage, TorrentStalled: e.torrentStalled, TorrentFound: e.torrentFound, TorrentPending: e.torrentPending}, true
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

// beginRun claims the next run of e: it installs a fresh cancelable
// context — before any waiting, so Pause can cancel a run that is only
// queued or still waiting for its slot — and bumps the run generation
// that keeps a stale worker's finish/clearCancel from touching a newer
// run. It refuses (ok=false) when the download is no longer Queued,
// e.g. it was paused before its worker reached it.
func (m *Manager) beginRun(e *entry) (context.Context, uint64, bool) {
	ctx, cancel := context.WithCancel(m.ctx)
	e.mu.Lock()
	if e.dl.Status != domain.StatusQueued {
		e.mu.Unlock()
		cancel()
		return nil, 0, false
	}
	e.runSeq++
	runID := e.runSeq
	e.runID = runID
	e.cancel = cancel
	e.mu.Unlock()
	return ctx, runID, true
}

// abandonRun drops a claimed run that never started: the download was
// paused, or the process is shutting down, while the worker waited for
// its start time or semaphore slot. A user pause is recorded as Paused;
// a shutdown leaves the status untouched so a Queued download starts
// again on the next run.
func (m *Manager) abandonRun(id string, runID uint64, ctx context.Context) {
	if m.ctx.Err() == nil && ctx.Err() != nil {
		m.finish(id, runID, ctx.Err(), ctx)
	}
	if e := m.getEntry(id); e != nil {
		e.clearCancel(runID)
	}
}

func (m *Manager) enqueue(id string) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		e := m.getEntry(id)
		if e == nil {
			return
		}
		ctx, runID, ok := m.beginRun(e)
		if !ok {
			return // paused (or otherwise no longer queued) before claim
		}

		e.mu.Lock()
		startAt := e.dl.StartAt
		e.mu.Unlock()
		if !startAt.IsZero() && time.Until(startAt) > 0 {
			timer := time.NewTimer(time.Until(startAt))
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				m.abandonRun(id, runID, ctx)
				return
			}
		}
		select {
		case m.sem <- struct{}{}:
		case <-ctx.Done():
			m.abandonRun(id, runID, ctx)
			return
		}
		defer func() { <-m.sem }()
		if ctx.Err() != nil {
			m.abandonRun(id, runID, ctx)
			return
		}

		e = m.getEntry(id)
		if e == nil {
			return
		}
		e.mu.Lock()
		kind := e.dl.EffectiveKind()
		e.mu.Unlock()

		if kind == domain.KindTorrent {
			m.runTorrentDownload(id, ctx, runID)
		} else {
			m.runDownload(id, ctx, runID)
		}
	}()
}

// runDownload drives one download from Queued through to a terminal
// (or Paused) state: probe if needed, split into segments, hand off to
// the engine, and aggregate the ProgressEvents it emits. The run was
// already claimed by beginRun — ctx and runID belong to it.
func (m *Manager) runDownload(id string, ctx context.Context, runID uint64) {
	e := m.getEntry(id)
	if e == nil {
		return
	}

	e.mu.Lock()
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
			// Reserve the name with O_EXCL so two concurrent probes of
			// the same filename can't both pick it.
			name, rerr := reserveName(m.downloadDir, e.dl.Filename)
			if rerr != nil {
				e.mu.Unlock()
				m.finish(id, runID, rerr, ctx)
				e.clearCancel(runID)
				return
			}
			e.dl.Dest = filepath.Join(m.downloadDir, name)
		}
		e.dl.TotalSize = res.Size
		e.dl.SupportsRange = res.SupportsRange
		e.dl.Segments = splitSegments(res.Size, e.dl.Connections, res.SupportsRange)
		e.mu.Unlock()
	}

	// Clamp persisted progress to the bytes that are actually on disk
	// (and to what the transport can resume) before the engine writes
	// at those offsets: a state file can outlive its destination.
	e.reconcileProgress()

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
// instantly without re-adding or re-verifying. The run was already
// claimed by beginRun — ctx and runID belong to it.
func (m *Manager) runTorrentDownload(id string, ctx context.Context, runID uint64) {
	e := m.getEntry(id)
	if e == nil {
		return
	}
	if m.torrentEngine == nil {
		e.fail(errors.New("torrent support is not configured"))
		m.persist(id)
		e.clearCancel(runID)
		return
	}

	e.setStatus(domain.StatusDownloading)
	m.persist(id)

	e.mu.Lock()
	runInput := e.dl.Clone()
	e.mu.Unlock()

	stats := make(chan TorrentStats, 16)
	done := make(chan error, 1)
	go func() { done <- m.torrentEngine.Start(ctx, id, runInput, stats) }()

	// Stats may arrive more often than UI samples. Calculate speed on a
	// fixed cadence so bursty piece events don't produce noisy rates.
	speedTicker := time.NewTicker(750 * time.Millisecond)
	defer speedTicker.Stop()
	persistTicker := time.NewTicker(2 * time.Second)
	defer persistTicker.Stop()

	for {
		select {
		case st := <-stats:
			m.applyTorrentStats(e, st)
		case <-speedTicker.C:
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
		// st.Name comes from untrusted torrent metadata (a magnet's dn
		// parameter included): keep Dest inside downloadDir so Remove
		// can never delete outside it.
		if dest, ok := joinWithin(m.downloadDir, st.Name); ok {
			e.dl.Dest = dest
		}
	}
	if st.TotalSize > 0 {
		e.dl.TotalSize = st.TotalSize
		e.dl.Segments = []domain.Segment{{Index: 0, Start: 0, End: st.TotalSize - 1, Downloaded: st.BytesDownloaded}}
	}
	e.speedBytes = st.BytesRead
	e.peers = st.Peers
	e.torrentFound = st.FoundPeers
	e.torrentPending = st.Pending
	e.torrentStage = st.Stage
	e.torrentStalled = st.Stalled
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
	// Trust, but verify: an HTTP run can return nil with bytes still
	// missing (a short body, a stripped Range). Completing anyway hands
	// the user a file with holes, so fail it here instead.
	if runErr == nil && e.dl.Kind == domain.KindHTTP && e.dl.TotalSize > 0 && e.dl.BytesDownloaded() < e.dl.TotalSize {
		runErr = fmt.Errorf("incomplete download: %d of %d bytes", e.dl.BytesDownloaded(), e.dl.TotalSize)
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
	if e.speedBytes > 0 {
		current = e.speedBytes
	}
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

// splitSegments divides size bytes across connections.
//
//   - size unknown → one unbounded segment (End = -1, "read until EOF");
//     ranged resume still works when the server advertises ranges.
//   - no range support → one bounded segment covering the whole file;
//     it is refetched from offset zero on every run, because a server
//     that cannot resume restarts its body at byte 0.
//   - otherwise → connections bounded segments.
func splitSegments(size int64, connections int, supportsRange bool) []domain.Segment {
	if size <= 0 {
		return []domain.Segment{{Index: 0, Start: 0, End: -1}}
	}
	if !supportsRange {
		return []domain.Segment{{Index: 0, Start: 0, End: size - 1}}
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

// reconcileProgress trims persisted segment progress back to what is
// actually on disk — and to what the transport can still resume —
// before a run writes at those offsets. State outlives its destination
// (manual delete, cleanup job, partial copy), and a server that no
// longer advertises ranges restarts its body from byte 0; resuming past
// either leaves a hole of zeros that still reports completion.
// Caller must not hold e.mu. Returns true when anything changed.
func (e *entry) reconcileProgress() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	changed := false
	if !e.dl.SupportsRange && !e.dl.Finished() {
		// No ranges → no resume: every byte is refetched from offset 0.
		for i := range e.dl.Segments {
			if e.dl.Segments[i].Downloaded != 0 {
				e.dl.Segments[i].Downloaded = 0
				changed = true
			}
		}
	}

	// Bytes a segment may still claim: at most the file's end, capped
	// at its own range. A missing file yields size - 1, i.e. zero.
	var size int64 = -1
	if e.dl.Dest != "" {
		if fi, err := os.Stat(e.dl.Dest); err == nil {
			size = fi.Size()
		}
	}
	for i := range e.dl.Segments {
		s := &e.dl.Segments[i]
		avail := size - s.Start
		if s.Bounded() && avail > s.Size() {
			avail = s.Size()
		}
		if avail < 0 {
			avail = 0
		}
		if s.Downloaded > avail {
			s.Downloaded = avail
			changed = true
		}
	}
	if changed {
		e.dl.UpdatedAt = time.Now()
	}
	return changed
}

// joinWithin joins name to dir and reports whether the result stays
// inside dir. name is untrusted (torrent metadata, state files).
func joinWithin(dir, name string) (string, bool) {
	dest := filepath.Join(dir, name)
	return dest, withinDir(dir, dest)
}

// withinDir reports whether path resolves strictly inside dir.
// dir itself counts as outside: Dest == downloadDir would delete the
// whole download directory on Remove (e.g. a torrent named ".").
func withinDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// reserveName appends " (n)" before the extension until it creates a
// file under that name with O_CREATE|O_EXCL, atomically reserving it —
// so a second download probing the same filename concurrently can't
// pick the same destination (the stat-then-use race uniqueName had).
// The empty placeholder file doubles as the download's destination;
// engine.Run opens and writes into it.
func reserveName(dir, name string) (string, error) {
	// The name can come from a Content-Disposition header, so strip any
	// directory a server tried to smuggle in ("../evil", "/etc/x").
	name = filepath.Base(filepath.Clean(name))
	if name == "" || name == "." || name == ".." || name == string(filepath.Separator) {
		name = "download"
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	candidate := name
	for i := 1; ; i++ {
		f, err := os.OpenFile(filepath.Join(dir, candidate), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if os.IsExist(err) {
			candidate = fmt.Sprintf("%s (%d)%s", base, i, ext)
			continue
		}
		if err != nil {
			return "", fmt.Errorf("reserving destination for %s: %w", name, err)
		}
		_ = f.Close()
		return candidate, nil
	}
}

// partDataRemains reports whether dest has leftover .part data: a
// sibling dest+".part", or any .part under dest when it's a directory
// (multi-file torrents promote parts inside the folder).
func partDataRemains(dest string) bool {
	if _, err := os.Stat(dest + ".part"); err == nil {
		return true
	}
	fi, err := os.Stat(dest)
	if err != nil || !fi.IsDir() {
		return false
	}
	found := false
	_ = filepath.WalkDir(dest, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(path, ".part") {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

func newID() string {
	return fmt.Sprintf("%d-%04x", time.Now().UnixNano(), rand.IntN(0x10000))
}
