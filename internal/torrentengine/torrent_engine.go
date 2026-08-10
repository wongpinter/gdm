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

// config tuning knobs derived from the anacrolix/torrent ClientConfig
// defaults (EstablishedConnsPerTorrent=55, HalfOpenConnsPerTorrent=25).
const (
	establishedConns = 80  // library default 55; bump for more peer fan-out
	halfOpenConns    = 40  // library default 25; faster initial peer acquisition
	peersHighWater   = 200 // library default 200; keep it
	peersLowWater    = 30  // library default 30
	pieceHashers     = 4   // library default 2; more cores → faster verification
)

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
	cfg.EstablishedConnsPerTorrent = establishedConns
	cfg.HalfOpenConnsPerTorrent = halfOpenConns
	cfg.TorrentPeersHighWater = peersHighWater
	cfg.TorrentPeersLowWater = peersLowWater
	cfg.PieceHashersPerTorrent = pieceHashers

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

	// Enable downloading — idempotent across pause/resume cycles.
	t.AllowDataDownload()

	// Sequential-first: raise priority of the first ~10% of pieces so
	// the file is usable as early as possible. DownloadAll covers the
	// rest at normal priority.
	if np := int(t.NumPieces()); np > 10 {
		stride := np / 10
		if stride < 10 {
			stride = 10
		}
		t.DownloadPieces(0, stride)
	}
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
	st := manager.TorrentStats{
		Name:            t.Name(),
		TotalSize:       t.Length(),
		BytesDownloaded: t.BytesCompleted(),
		Peers:           len(t.PeerConns()),
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
	if e.torrents == nil {
		e.torrents = make(map[string]*torrent.Torrent)
	}
	t, exists := e.torrents[id]
	e.mu.Unlock()
	if exists {
		return t, nil
	}
	t, err := e.addTorrent(uri)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.torrents[id] = t
	e.mu.Unlock()
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
