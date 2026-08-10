package manager

import (
	"context"

	"github.com/wongpinter/gdm/internal/domain"
)

// Store persists download state so the queue survives a restart.
// Defined here, in the consumer, per Go's interface convention —
// the JSON-file adapter in internal/store implements it implicitly.
type Store interface {
	Save(d *domain.Download) error
	LoadAll() ([]*domain.Download, error)
	Delete(id string) error
}

// ProgressEvent is emitted by the engine as bytes land on disk.
// It carries a delta, not a running total, so the manager can
// aggregate without asking the engine to know about Download.
type ProgressEvent struct {
	SegmentIndex int
	BytesWritten int64
}

// Engine performs the actual network I/O for one download. The manager
// owns concurrency and persistence; the engine only knows how to probe
// a URL and stream bytes into segments.
type Engine interface {
	// Probe issues a HEAD (falling back to a ranged GET) to discover
	// size, filename, and whether byte-range requests are supported.
	Probe(ctx context.Context, url string) (ProbeResult, error)

	// Run downloads every unfinished segment of d concurrently,
	// writing directly into d.Dest at the correct offsets. It sends a
	// ProgressEvent after every chunk written and returns when all
	// segments are done, ctx is canceled (pause), or an error occurs.
	Run(ctx context.Context, d *domain.Download, events chan<- ProgressEvent) error
}

// ProbeResult is what Engine.Probe learns about a URL before any
// segment is created.
type ProbeResult struct {
	Filename      string
	Size          int64 // -1 if unknown
	SupportsRange bool
}

// TorrentStats is a snapshot the torrent engine emits whenever a piece
// completes or the peer count changes (at most every 750ms).
type TorrentStats struct {
	Name            string
	TotalSize       int64
	BytesDownloaded int64
	Peers           int
	Done            bool
}

// TorrentEngine performs BitTorrent downloads: given a magnet URI or a
// path to a .torrent file, it joins the swarm, fetches metadata, and
// streams TorrentStats until the transfer completes or ctx is canceled.
//
// Torrents stay in the client across pause/resume cycles so a resume is
// instant — no re-add, no re-verify. Remove(id) is the only path that
// drops a torrent from the swarm. Unlike Engine, it doesn't work in
// Segments — a torrent's pieces are the library's concern, not the
// manager's — so it gets its own small port.
type TorrentEngine interface {
	// Start joins the swarm for id if first call, or resumes the
	// existing torrent if id was previously paused. It streams
	// TorrentStats as pieces land and returns nil when the torrent
	// completes, or ctx.Err() when the caller cancels (pause).
	Start(ctx context.Context, id string, d *domain.Download, stats chan<- TorrentStats) error

	// Remove drops the torrent from the client and forgets it.
	// Safe to call regardless of whether the torrent is active.
	Remove(id string)

	// Close shuts down the torrent client entirely.
	Close() error
}
