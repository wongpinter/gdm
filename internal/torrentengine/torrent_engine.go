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
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"

	"github.com/wongpinter/gdm/internal/domain"
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
type Engine struct {
	client   *torrent.Client
	dataDir  string
	mu       sync.Mutex
	torrents map[string]*torrent.Torrent // id → active torrent
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

	// Wait for metadata if not already available.
	select {
	case <-t.GotInfo():
	case <-ctx.Done():
		return ctx.Err()
	}

	// Add protocol-diverse fallbacks only after BEP 27 metadata proves
	// this isn't a private torrent. AddTrackers deduplicates endpoints
	// already present in the magnet or .torrent file.
	if info := t.Info(); info != nil && (info.Private == nil || !*info.Private) {
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

// streamStats sends an initial TorrentStats snapshot and then streams
// updates on every piece-completion event (with a fallback ticker for
// stretches where no piece changes — initial peer discovery, stalls).
func (e *Engine) streamStats(ctx context.Context, t *torrent.Torrent, stats chan<- manager.TorrentStats) error {
	if !sendStats(ctx, t, stats) {
		return ctx.Err()
	}

	sub := t.SubscribePieceStateChanges()
	defer sub.Close()

	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-sub.Values:
			if !sendStats(ctx, t, stats) {
				return ctx.Err()
			}
		case <-ticker.C:
			if !sendStats(ctx, t, stats) {
				return ctx.Err()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// sendStats emits one TorrentStats snapshot. Returns false if the
// context is done (caller should return ctx.Err()).
func sendStats(ctx context.Context, t *torrent.Torrent, stats chan<- manager.TorrentStats) bool {
	done := t.Complete().Bool()
	torrentStats := t.Stats()
	st := manager.TorrentStats{
		Name:            t.Name(),
		TotalSize:       t.Length(),
		BytesDownloaded: t.BytesCompleted(),
		BytesRead:       torrentStats.BytesReadUsefulData.Int64(),
		Peers:           torrentStats.ActivePeers,
		Done:            done,
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
