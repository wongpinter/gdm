package domain

import (
	"testing"
	"time"
)

// A segment with End < 0 (unknown length) must never report Done: the
// old Downloaded >= Size() form made Size()==0 mean "complete", so an
// unknown-size download was marked completed with a 0-byte file
// without the transport ever reading a byte.
func TestSegmentDoneOnlyWhenBoundedAndFull(t *testing.T) {
	full := Segment{Start: 0, End: 99, Downloaded: 100}
	if !full.Done() {
		t.Error("fully fetched bounded segment not Done")
	}
	partial := Segment{Start: 100, End: 199, Downloaded: 99}
	if partial.Done() {
		t.Error("partially fetched bounded segment reported Done")
	}
	never := Segment{Start: 0, End: -1}
	if never.Bounded() {
		t.Error("unbounded segment reported Bounded")
	}
	if got := never.Size(); got != 0 {
		t.Errorf("unbounded Size = %d, want 0", got)
	}
	if never.Done() {
		t.Error("unbounded segment reported Done before any fetch")
	}
}

func TestDownloadBytesDownloadedAndProgress(t *testing.T) {
	d := &Download{
		TotalSize: 100,
		Segments: []Segment{
			{Index: 0, Start: 0, End: 49, Downloaded: 30},
			{Index: 1, Start: 50, End: 99, Downloaded: 20},
		},
	}
	if got := d.BytesDownloaded(); got != 50 {
		t.Errorf("BytesDownloaded = %d, want 50", got)
	}
	if got := d.Progress(); got != 0.5 {
		t.Errorf("Progress = %v, want 0.5", got)
	}

	unknown := &Download{TotalSize: 0, Segments: []Segment{{Start: 0, End: -1}}}
	if got := unknown.Progress(); got != 0 {
		t.Errorf("Progress with unknown total = %v, want 0", got)
	}
}

func TestDownloadCloneIsDeep(t *testing.T) {
	orig := &Download{
		ID:        "x",
		Status:    StatusDownloading,
		CreatedAt: time.Now(),
		Segments:  []Segment{{Index: 0, Start: 0, End: 9, Downloaded: 5}},
	}
	c := orig.Clone()
	c.Status = StatusCompleted
	c.Segments[0].Downloaded = 9
	if orig.Status != StatusDownloading {
		t.Error("Clone aliased Status")
	}
	if orig.Segments[0].Downloaded != 5 {
		t.Error("Clone shares the segment backing array")
	}
}

func TestDownloadActiveAndFinished(t *testing.T) {
	for _, s := range []Status{StatusQueued, StatusProbing, StatusDownloading} {
		d := &Download{Status: s}
		if !d.Active() || d.Finished() {
			t.Errorf("status %s: Active=%v Finished=%v, want active+unfinished", s, d.Active(), d.Finished())
		}
	}
	for _, s := range []Status{StatusPaused, StatusCompleted, StatusFailed, StatusCanceled} {
		d := &Download{Status: s}
		if d.Active() {
			t.Errorf("status %s reported Active", s)
		}
	}
	for _, s := range []Status{StatusCompleted, StatusFailed, StatusCanceled} {
		if !(&Download{Status: s}).Finished() {
			t.Errorf("status %s not Finished", s)
		}
	}
}
