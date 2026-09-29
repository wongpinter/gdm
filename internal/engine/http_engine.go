// Package engine is the outbound adapter that talks HTTP. It knows
// nothing about the manager's queue or persistence — it only probes
// URLs and streams bytes into a file at the offsets it's told to use.
package engine

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/wongpinter/gdm/internal/domain"
	"github.com/wongpinter/gdm/internal/manager"
)

const readBufSize = 32 * 1024

// HTTPEngine implements manager.Engine over net/http.
type HTTPEngine struct {
	client *http.Client
}

// New returns an HTTPEngine with a client tuned for large, long-lived
// transfers (no overall timeout — progress, not deadline, governs them).
func New() *HTTPEngine {
	return &HTTPEngine{
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

var _ manager.Engine = (*HTTPEngine)(nil)

// Probe issues a HEAD request to learn size, filename, and range
// support. Some servers reject HEAD, so it falls back to a single-byte
// ranged GET, which is cheap and works almost everywhere.
func (e *HTTPEngine) Probe(ctx context.Context, rawURL string) (manager.ProbeResult, error) {
	res, err := e.probeWith(ctx, http.MethodHead, rawURL)
	if err == nil && res.Size >= 0 {
		return res, nil
	}
	return e.probeWith(ctx, http.MethodGet, rawURL)
}

func (e *HTTPEngine) probeWith(ctx context.Context, method, rawURL string) (manager.ProbeResult, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return manager.ProbeResult{}, fmt.Errorf("building probe request: %w", err)
	}
	if method == http.MethodGet {
		// Ask for one byte so we don't pull the whole body just to
		// inspect headers, but still see Accept-Ranges / a 206.
		req.Header.Set("Range", "bytes=0-0")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return manager.ProbeResult{}, fmt.Errorf("probing %s: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, readBufSize))

	if resp.StatusCode >= 400 {
		return manager.ProbeResult{}, fmt.Errorf("probing %s: server returned %s", rawURL, resp.Status)
	}

	result := manager.ProbeResult{
		Size:          -1,
		SupportsRange: resp.StatusCode == http.StatusPartialContent || resp.Header.Get("Accept-Ranges") == "bytes",
		Filename:      filenameFrom(rawURL, resp.Header.Get("Content-Disposition")),
	}

	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if idx := strings.LastIndex(cr, "/"); idx != -1 && cr[idx+1:] != "*" {
			if n, err := strconv.ParseInt(cr[idx+1:], 10, 64); err == nil {
				result.Size = n
			}
		}
	} else if cl := resp.Header.Get("Content-Length"); cl != "" && resp.StatusCode == http.StatusOK {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			result.Size = n
		}
	}
	return result, nil
}

func filenameFrom(rawURL, contentDisposition string) string {
	if _, params, err := mime.ParseMediaType(contentDisposition); err == nil {
		if name := params["filename"]; name != "" {
			return path.Base(name)
		}
	}
	if u, err := url.Parse(rawURL); err == nil {
		if base := path.Base(u.Path); base != "" && base != "." && base != "/" {
			return base
		}
	}
	return "download"
}

// Run downloads every unfinished segment of d concurrently. It assumes
// d.Segments and d.Dest are already populated by the manager, creates
// d.Dest if missing, and pre-sizes it to d.TotalSize when known — one
// truncate up front instead of the file growing with every WriteAt.
func (e *HTTPEngine) Run(ctx context.Context, d *domain.Download, events chan<- manager.ProgressEvent) error {
	file, err := os.OpenFile(d.Dest, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", d.Dest, err)
	}
	defer func() { _ = file.Close() }()

	if d.TotalSize > 0 {
		// Pre-size while no segment is writing yet: the filesystem
		// allocates the full extent in one call (and a full disk fails
		// here, up front, instead of mid-transfer at an arbitrary offset).
		if err := file.Truncate(d.TotalSize); err != nil {
			return fmt.Errorf("pre-sizing %s: %w", d.Dest, err)
		}
	}

	errCh := make(chan error, len(d.Segments))
	pending := 0
	// First segment error cancels the run: sibling connections stop
	// instead of continuing to fill a destination whose run already
	// failed (the loop below still drains every goroutine).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i := range d.Segments {
		seg := d.Segments[i]
		if seg.Done() {
			continue
		}
		pending++
		go func(seg domain.Segment) {
			errCh <- e.runSegment(ctx, d, seg, file, events)
		}(seg)
	}

	var firstErr error
	for range pending {
		if err := <-errCh; err != nil && firstErr == nil {
			firstErr = err
			cancel()
		}
	}
	return firstErr
}

func (e *HTTPEngine) runSegment(ctx context.Context, d *domain.Download, seg domain.Segment, file *os.File, events chan<- manager.ProgressEvent) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return fmt.Errorf("segment %d: %w", seg.Index, err)
	}
	if d.SupportsRange {
		upper := ""
		if seg.End >= 0 {
			upper = strconv.FormatInt(seg.End, 10)
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%s", seg.NextOffset(), upper))
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("segment %d: %w", seg.Index, err)
	}
	defer func() { _ = resp.Body.Close() }()

	offset := seg.NextOffset()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		if start, ok := contentRangeStart(resp.Header.Get("Content-Range")); ok && start != offset {
			return fmt.Errorf("segment %d: server resumed at byte %d, want %d", seg.Index, start, offset)
		}
	case http.StatusOK:
		// A 200 body starts at byte 0 of the resource, which only fits
		// when this run also starts at byte 0 (one segment, offset 0).
		// Anything else writes the head of the file over its middle —
		// this is what a proxy that strips the Range header looks like.
		if offset != 0 || len(d.Segments) != 1 {
			return fmt.Errorf("segment %d: server ignored the Range request (200); refusing to write at offset %d", seg.Index, offset)
		}
	default:
		return fmt.Errorf("segment %d: server returned %s", seg.Index, resp.Status)
	}

	buf := make([]byte, readBufSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := file.WriteAt(buf[:n], offset); werr != nil {
				return fmt.Errorf("segment %d: writing to disk: %w", seg.Index, werr)
			}
			offset += int64(n)
			select {
			case events <- manager.ProgressEvent{SegmentIndex: seg.Index, BytesWritten: int64(n)}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				// EOF is success only for a segment that reads until
				// EOF; a bounded segment ending early means bytes
				// nobody would ever come back for.
				if seg.Bounded() && offset < seg.End+1 {
					return fmt.Errorf("segment %d: server closed the connection after %d of %d bytes", seg.Index, offset-seg.Start, seg.Size())
				}
				return nil
			}
			return fmt.Errorf("segment %d: %w", seg.Index, rerr)
		}
	}
}

// contentRangeStart parses the start offset of a Content-Range header
// ("bytes 100-199/2000"); ok is false when the header is absent or has
// no parseable start.
func contentRangeStart(v string) (int64, bool) {
	if !strings.HasPrefix(v, "bytes ") {
		return 0, false
	}
	rest := v[len("bytes "):]
	i := strings.IndexByte(rest, '-')
	if i < 0 {
		return 0, false
	}
	start, err := strconv.ParseInt(rest[:i], 10, 64)
	if err != nil {
		return 0, false
	}
	return start, true
}
