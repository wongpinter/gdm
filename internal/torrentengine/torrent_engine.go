// Package torrentengine is the outbound adapter for BitTorrent
// downloads. It wraps a single, shared anacrolix/torrent.Client (swarm
// connections and DHT bootstrapping are expensive to redo per download),
// subscribes to piece-completion events instead of polling, and keeps
// torrents in the client across pause/resume cycles so a resume is
// instant — no re-add, no re-verify.
package torrentengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/wongpinter/gdm/internal/domain"
	"github.com/wongpinter/gdm/internal/magnet"
	"github.com/wongpinter/gdm/internal/manager"
)

// fallbackTrackers supplement stale tracker lists after metadata proves
// the torrent is public. HTTPS matters on Colab and similar networks
// that often block UDP; separate tiers let each endpoint announce.
// Private torrents never receive these trackers (BEP 27).
var fallbackTrackers = [][]string{
	{"https://tracker.opentrackr.org:443/announce"},
	{"udp://tracker.opentrackr.org:1337/announce"},
	{"udp://tracker.openbittorrent.com:6969/announce"},
}

// Engine implements manager.TorrentEngine over a shared torrent.Client.
// Torrents are keyed by manager-assigned download ID and stay in the
// client until Remove is called — pausing only calls DisallowDataDownload
// to stop piece requests, keeping the swarm connection and piece state alive.
//
// metaDir and allowPublicTrackers are set once at startup via the
// setters below and never mutated while downloads run.
type Engine struct {
	client   *torrent.Client
	dataDir  string
	metaDir  string
	mu       sync.Mutex
	torrents map[string]*torrent.Torrent // id → active torrent
	// allowPublicTrackers opts into announcing unknown info hashes to
	// public fallback trackers during metadata fetch. Off by default:
	// without it, fallback trackers are only added after metadata proves
	// the torrent isn't private (BEP 27).
	allowPublicTrackers bool
}

const (
	// metadataTimeout bounds magnet metadata fetch so a dead swarm
	// fails loudly instead of pinning a worker at 0/0 bytes forever.
	metadataTimeout = 10 * time.Minute
	// stallAfter marks a post-metadata torrent stalled after this long
	// without payload progress. Discovery legitimately takes a while,
	// so this is generous.
	stallAfter = 90 * time.Second
	// metaStageTick is the metadata-phase heartbeat. UI shows a live
	// "fetching metadata" stage instead of a frozen 0/0 line.
	metaStageTick = 2 * time.Second
)

// SetMetainfoCache enables the metainfo cache: fetched torrent info is
// persisted under dir as <infohash>.torrent and reused on later adds,
// skipping a second metadata fetch after restarts.
func (e *Engine) SetMetainfoCache(dir string) {
	e.metaDir = dir
}

// SetAllowPublicTrackers opts into pre-metadata fallback trackers.
// See allowPublicTrackers docs for the privacy tradeoff.
func (e *Engine) SetAllowPublicTrackers(allow bool) {
	e.allowPublicTrackers = allow
}

// shouldAddFallback reports whether fallback trackers may be added for
// metadata info: only public torrents (BEP 27 private flag unset/false).
func shouldAddFallback(info *metainfo.Info) bool {
	return info != nil && (info.Private == nil || !*info.Private)
}

var _ manager.TorrentEngine = (*Engine)(nil)

// New creates the client and points its default file storage at dataDir
// — the same directory HTTP downloads land in, so both kinds show up
// side by side. Call Close when the manager shuts down.
func New(dataDir string) (*Engine, error) {
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dataDir
	cfg.Seed = true // keep pieces available for resume verification
	// Keep anacrolix's balanced connection, peer-watermark, hashing,
	// and unverified-byte defaults. Earlier overrides raised fan-out and
	// hashing while lowering the peer pool; that caused CPU/socket churn
	// and less consistent throughput on constrained Colab runtimes.

	client, err := torrent.NewClient(cfg)
	if err != nil {
		// Retry with IPv6 disabled — some minimal containers and VPS
		// images have no IPv6 stack at all.
		cfg.DisableIPv6 = true
		client, err = torrent.NewClient(cfg)
		if err != nil {
			return nil, fmt.Errorf("starting torrent client: %w", err)
		}
	}
	return &Engine{
		client:   client,
		dataDir:  dataDir,
		torrents: make(map[string]*torrent.Torrent),
	}, nil
}

// Close releases the swarm client's listening sockets and DHT state.
func (e *Engine) Close() error {
	errs := e.client.Close()
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("closing torrent client: %v", errs)
}

// Start joins (or resumes) the swarm for id. On first call it fetches
// metadata, starts downloading all files, and streams stats until
// completion or ctx cancellation. On resume it just calls
// AllowDataDownload to re-enable piece requests and re-enters the event
// loop — no re-add, no re-verify.
func (e *Engine) Start(ctx context.Context, id string, d *domain.Download, stats chan<- manager.TorrentStats) error {
	t, err := e.getOrAdd(id, d.URL)
	if err != nil {
		return err
	}

	// Opt-in rescue for magnets whose own trackers are dead: announce
	// the unknown info hash to public fallbacks while fetching
	// metadata. Default off — see allowPublicTrackers docs.
	if e.allowPublicTrackers && strings.HasPrefix(d.URL, "magnet:") {
		t.AddTrackers(fallbackTrackers)
	}

	// Wait for metadata, emitting a live stage so UI shows "fetching
	// metadata" instead of a frozen 0/0-bytes line. Fails loudly on
	// dead swarms rather than pinning a worker forever.
	if err := e.waitMetadata(ctx, t, d.URL, stats); err != nil {
		return err
	}

	// Persist metainfo so restarts skip the metadata fetch (best
	// effort — a cache failure must never fail the download).
	e.saveMetainfo(t)

	// Add protocol-diverse fallbacks only after BEP 27 metadata proves
	// this isn't a private torrent. AddTrackers deduplicates endpoints
	// already present in the magnet or .torrent file.
	if shouldAddFallback(t.Info()) {
		t.AddTrackers(fallbackTrackers)
	}

	// Enable downloading — idempotent across pause/resume cycles.
	// Let anacrolix's rarest-first request strategy choose all pieces;
	// prioritizing an initial slice reduces swarm diversity and can stall
	// when few peers own those pieces.
	t.AllowDataDownload()
	t.DownloadAll()

	err = e.streamStats(ctx, t, stats)

	// Pause: stop requesting new pieces, but keep swarm connection
	// and piece state. Resume will call AllowDataDownload again.
	t.DisallowDataDownload()

	return err
}

// waitMetadata blocks until torrent info arrives, emitting a metadata
// stage heartbeat so UI shows live discovery state. Returns a timeout
// error when no peer serves metadata within metadataTimeout.
func (e *Engine) waitMetadata(ctx context.Context, t *torrent.Torrent, uri string, stats chan<- manager.TorrentStats) error {
	select {
	case <-t.GotInfo():
		return nil
	default:
	}
	name := magnet.DisplayNameOf(uri)
	if name == "" {
		name = t.Name()
	}
	send := func() bool {
		st := manager.TorrentStats{
			Name:  name,
			Peers: len(t.PeerConns()),
			Stage: manager.StageMetadata,
		}
		select {
		case stats <- st:
			return true
		case <-t.GotInfo():
			return true
		case <-ctx.Done():
			return false
		}
	}
	if !send() {
		return ctx.Err()
	}
	deadline := time.Now().Add(metadataTimeout)
	ticker := time.NewTicker(metaStageTick)
	defer ticker.Stop()
	for {
		select {
		case <-t.GotInfo():
			return nil
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("metadata timeout after %s: no peers serving torrent info", metadataTimeout)
			}
			if !send() {
				return ctx.Err()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// saveMetainfo persists fetched torrent info as <infohash>.torrent so
// later adds skip the metadata fetch. Best effort by design.
func (e *Engine) saveMetainfo(t *torrent.Torrent) {
	if e.metaDir == "" {
		return
	}
	ih := strings.ToLower(t.InfoHash().HexString())
	if ih == "" {
		return
	}
	if err := os.MkdirAll(e.metaDir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(e.metaDir, ih+"-*.torrent")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	mi := t.Metainfo()
	if err := mi.Write(tmp); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	os.Rename(tmpName, filepath.Join(e.metaDir, ih+".torrent"))
}

// streamStats sends an initial TorrentStats snapshot and then streams
// updates on every piece-completion event (with a fallback ticker for
// stretches where no piece changes — initial peer discovery, stalls).
// A torrent with no payload progress for stallAfter reports Stalled so
// UI distinguishes a stuck swarm from healthy discovery.
func (e *Engine) streamStats(ctx context.Context, t *torrent.Torrent, stats chan<- manager.TorrentStats) error {
	lastBytes := t.BytesCompleted()
	lastChange := time.Now()
	if !sendStats(ctx, t, stats, manager.StageDownloading, false) {
		return ctx.Err()
	}

	sub := t.SubscribePieceStateChanges()
	defer sub.Close()

	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()

	refresh := func() bool {
		now := time.Now()
		if done := t.BytesCompleted(); done != lastBytes {
			lastBytes = done
			lastChange = now
		}
		return sendStats(ctx, t, stats, manager.StageDownloading, now.Sub(lastChange) > stallAfter)
	}
	for {
		select {
		case <-sub.Values:
			if !refresh() {
				return ctx.Err()
			}
		case <-ticker.C:
			if !refresh() {
				return ctx.Err()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// sendStats emits one TorrentStats snapshot. Returns false if the
// context is done (caller should return ctx.Err()).
func sendStats(ctx context.Context, t *torrent.Torrent, stats chan<- manager.TorrentStats, stage string, stalled bool) bool {
	done := t.Complete().Bool()
	torrentStats := t.Stats()
	st := manager.TorrentStats{
		Name:            t.Name(),
		TotalSize:       t.Length(),
		BytesDownloaded: t.BytesCompleted(),
		BytesRead:       torrentStats.BytesReadUsefulData.Int64(),
		Peers:           torrentStats.ActivePeers,
		Done:            done,
		Stage:           stage,
		Stalled:         stalled && !done,
	}
	select {
	case stats <- st:
		return !done
	case <-ctx.Done():
		return false
	}
}

// Remove drops the torrent from the client and forgets it. Safe to call
// regardless of whether the id is known.
func (e *Engine) Remove(id string) {
	e.mu.Lock()
	t, ok := e.torrents[id]
	if ok {
		delete(e.torrents, id)
	}
	e.mu.Unlock()
	if ok {
		t.Drop()
	}
}

// ---------- internal helpers ----------

// getOrAdd returns the existing torrent for id, or adds and returns a
// new one. The returned torrent is guaranteed non-nil if err is nil.
func (e *Engine) getOrAdd(id, uri string) (*torrent.Torrent, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.torrents == nil {
		e.torrents = make(map[string]*torrent.Torrent)
	}
	if t, ok := e.torrents[id]; ok {
		return t, nil
	}
	t, err := e.addTorrent(uri)
	if err != nil {
		return nil, err
	}
	e.torrents[id] = t
	return t, nil
}

func (e *Engine) addTorrent(uri string) (*torrent.Torrent, error) {
	if strings.HasPrefix(uri, "magnet:") {
		// Prefer cached metainfo from an earlier fetch: the full info
		// is already known, so no metadata round trip is needed. Falls
		// back to the magnet when uncached or unreadable.
		if path := e.cachedMetainfo(uri); path != "" {
			if t, err := e.client.AddTorrentFromFile(path); err == nil {
				return t, nil
			}
		}
		t, err := e.client.AddMagnet(uri)
		if err != nil {
			return nil, fmt.Errorf("adding magnet: %w", err)
		}
		return t, nil
	}
	t, err := e.client.AddTorrentFromFile(uri)
	if err != nil {
		return nil, fmt.Errorf("adding torrent file %s: %w", uri, err)
	}
	return t, nil
}

// cachedMetainfo returns the path of a cached .torrent for uri's info
// hash, or "" when caching is off, unparseable, or not yet fetched.
func (e *Engine) cachedMetainfo(uri string) string {
	if e.metaDir == "" {
		return ""
	}
	ih, err := magnet.InfoHashOf(uri)
	if err != nil || ih == "" {
		return ""
	}
	path := filepath.Join(e.metaDir, ih+".torrent")
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		return ""
	}
	return path
}
