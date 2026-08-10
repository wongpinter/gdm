// Package domain holds the core entities of the download manager.
// Nothing in this package touches the network, disk, or a terminal —
// that's the point of keeping it at the center of the hexagon.
package domain

import "time"

// Status is the lifecycle state of a Download.
type Status string

const (
	StatusQueued      Status = "queued"
	StatusProbing     Status = "probing"
	StatusDownloading Status = "downloading"
	StatusPaused      Status = "paused"
	StatusCompleted   Status = "completed"
	StatusFailed      Status = "failed"
	StatusCanceled    Status = "canceled"
)

// Kind distinguishes how a Download is fetched. The zero value ("")
// is treated as KindHTTP for backward compatibility with state files
// written before torrent support existed.
type Kind string

const (
	KindHTTP    Kind = "http"
	KindTorrent Kind = "torrent"
)

// Segment is one byte-range slice of a download, handled by a single
// HTTP connection. Start/End are inclusive, absolute file offsets.
// Downloaded is how many bytes of [Start, End] have been written so
// far, which lets a segment resume mid-range after a pause or crash.
type Segment struct {
	Index      int   `json:"index"`
	Start      int64 `json:"start"`
	End        int64 `json:"end"`
	Downloaded int64 `json:"downloaded"`
}

// Size returns the total byte length this segment is responsible for.
func (s Segment) Size() int64 {
	return s.End - s.Start + 1
}

// Done reports whether this segment has fetched every byte in its range.
func (s Segment) Done() bool {
	return s.Downloaded >= s.Size()
}

// NextOffset is the absolute file offset to resume this segment from.
func (s Segment) NextOffset() int64 {
	return s.Start + s.Downloaded
}

// Download is the aggregate root: everything the manager and the UI
// need to know about one file transfer.
type Download struct {
	ID  string `json:"id"`
	URL string `json:"url"` // http(s) URL, magnet URI, or path to a .torrent file
	// Kind is normalized via EffectiveKind(); the zero value means HTTP.
	Kind          Kind      `json:"kind,omitempty"`
	Filename      string    `json:"filename"`
	Dest          string    `json:"dest"` // absolute path on disk (a directory for torrents)
	TotalSize     int64     `json:"total_size"`
	SupportsRange bool      `json:"supports_range"`
	Connections   int       `json:"connections"`
	Segments      []Segment `json:"segments"`
	Status        Status    `json:"status"`
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// EffectiveKind returns Kind, treating the zero value as KindHTTP so
// state files written before torrent support existed keep working.
func (d *Download) EffectiveKind() Kind {
	if d.Kind == "" {
		return KindHTTP
	}
	return d.Kind
}

// BytesDownloaded sums the progress of every segment.
func (d *Download) BytesDownloaded() int64 {
	var sum int64
	for _, s := range d.Segments {
		sum += s.Downloaded
	}
	return sum
}

// Progress returns completion in [0,1]. Zero when total size is unknown.
func (d *Download) Progress() float64 {
	if d.TotalSize <= 0 {
		return 0
	}
	return float64(d.BytesDownloaded()) / float64(d.TotalSize)
}

// Active reports whether the download is currently occupying a worker slot.
func (d *Download) Active() bool {
	switch d.Status {
	case StatusQueued, StatusProbing, StatusDownloading:
		return true
	default:
		return false
	}
}

// Finished reports whether the download has reached a terminal state.
func (d *Download) Finished() bool {
	switch d.Status {
	case StatusCompleted, StatusFailed, StatusCanceled:
		return true
	default:
		return false
	}
}

// Clone returns a deep copy, safe to hand to a reader (e.g. the TUI)
// while the manager keeps mutating its own copy under a lock.
func (d *Download) Clone() *Download {
	cp := *d
	cp.Segments = append([]Segment(nil), d.Segments...)
	return &cp
}
