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
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, readBufSize))

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
// d.Segments and d.Dest are already populated by the manager and that
// the destination file exists (pre-sized when the total is known).
func (e *HTTPEngine) Run(ctx context.Context, d *domain.Download, events chan<- manager.ProgressEvent) error {
	file, err := os.OpenFile(d.Dest, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", d.Dest, err)
	}
	defer file.Close()

	errCh := make(chan error, len(d.Segments))
	pending := 0
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("segment %d: server returned %s", seg.Index, resp.Status)
	}

	offset := seg.NextOffset()
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
				return nil
			}
			return fmt.Errorf("segment %d: %w", seg.Index, rerr)
		}
	}
}
