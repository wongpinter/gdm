package manager

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wongpinter/gdm/internal/domain"
)

type lifecycleStore struct{}

func (lifecycleStore) Save(*domain.Download) error          { return nil }
func (lifecycleStore) LoadAll() ([]*domain.Download, error) { return nil, nil }
func (lifecycleStore) Delete(string) error                  { return nil }

type failingLifecycleStore struct{ lifecycleStore }

func (failingLifecycleStore) Save(*domain.Download) error { return context.DeadlineExceeded }

func TestAddRemovesEntryWhenInitialSaveFails(t *testing.T) {
	m := &Manager{entries: make(map[string]*entry), store: failingLifecycleStore{}}
	dl := &domain.Download{ID: "id", Status: domain.StatusQueued}
	if _, err := m.add(dl); err == nil {
		t.Fatal("add succeeded with failing store")
	}
	if _, ok := m.Get(dl.ID); ok {
		t.Fatal("failed add left entry in manager")
	}
}

func TestFinishCanceledRunBecomesPaused(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := &entry{dl: &domain.Download{Status: domain.StatusProbing}, runID: 1}
	m := &Manager{entries: map[string]*entry{"id": e}, store: lifecycleStore{}}
	m.finish("id", 1, context.Canceled, ctx)
	if e.dl.Status != domain.StatusPaused {
		t.Fatalf("status = %q, want paused", e.dl.Status)
	}
}

func TestTorrentSpeedUsesMonotonicNetworkBytes(t *testing.T) {
	now := time.Now()
	e := &entry{
		dl:         &domain.Download{TotalSize: 100, Segments: []domain.Segment{{End: 99, Downloaded: 80}}},
		lastSample: now.Add(-time.Second),
		lastBytes:  1_000,
		speedBytes: 2_000,
	}
	e.updateSpeed()
	if e.speed <= 0 {
		t.Fatalf("speed = %v, want positive rate from monotonic network bytes", e.speed)
	}

	// Completed piece bytes can decrease after hash failure; network
	// bytes remain monotonic and must continue producing a sane rate.
	e.dl.Segments[0].Downloaded = 10
	e.lastSample = time.Now().Add(-time.Second)
	e.speedBytes = 3_000
	e.updateSpeed()
	if e.speed <= 0 {
		t.Fatalf("speed after hash failure = %v, want positive", e.speed)
	}
}

func TestClearCancelDoesNotClobberNewRun(t *testing.T) {
	_, oldCancel := context.WithCancel(context.Background())
	defer oldCancel()
	newCtx, newCancel := context.WithCancel(context.Background())
	defer newCancel()

	e := &entry{}
	e.runSeq = 1
	e.runID = 1
	e.cancel = oldCancel

	e.runSeq++
	e.runID = e.runSeq
	e.cancel = newCancel
	e.clearCancel(1)

	e.mu.Lock()
	got := e.cancel
	e.mu.Unlock()
	if got == nil {
		t.Fatal("stale run cleared cancel function for newer run")
	}

	newCancel()
	select {
	case <-newCtx.Done():
	default:
		t.Fatal("new run cancel function is not active")
	}
}

// TestResumeRollsBackWhenSaveFails pins that a failed persist doesn't
// leave the entry claiming Queued in memory while nothing was saved
// and no worker was scheduled.
func TestResumeRollsBackWhenSaveFails(t *testing.T) {
	e := &entry{dl: &domain.Download{ID: "id", Status: domain.StatusPaused, Error: "boom"}}
	m := &Manager{entries: map[string]*entry{"id": e}, store: failingLifecycleStore{}}

	if err := m.Resume("id"); err == nil {
		t.Fatal("Resume succeeded with failing store")
	}
	e.mu.Lock()
	status, errMsg := e.dl.Status, e.dl.Error
	e.mu.Unlock()
	if status != domain.StatusPaused || errMsg != "boom" {
		t.Fatalf("after failed Resume: status=%q error=%q, want paused/boom", status, errMsg)
	}
}

// TestPauseQueuedBeforeClaim pins the pre-beginRun pause path: no
// worker holds a cancel function yet, so Pause flips the status
// directly and beginRun then refuses to start the download.
func TestPauseQueuedBeforeClaim(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &entry{dl: &domain.Download{ID: "id", Status: domain.StatusQueued}}
	m := &Manager{
		entries: map[string]*entry{"id": e},
		store:   lifecycleStore{},
		ctx:     ctx,
		cancel:  cancel,
	}

	if err := m.Pause("id"); err != nil {
		t.Fatalf("Pause on queued download: %v", err)
	}
	e.mu.Lock()
	status := e.dl.Status
	e.mu.Unlock()
	if status != domain.StatusPaused {
		t.Fatalf("status = %q, want paused", status)
	}
	if _, _, ok := m.beginRun(e); ok {
		t.Fatal("beginRun claimed a paused download")
	}
}

// TestAbandonRun pins how a claimed-but-never-started run ends: a user
// pause is recorded as Paused, while a manager shutdown leaves the
// status Queued so the download restarts on the next run.
func TestAbandonRun(t *testing.T) {
	t.Run("user pause becomes paused", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		e := &entry{dl: &domain.Download{ID: "id", Status: domain.StatusQueued}, runID: 1}
		m := &Manager{entries: map[string]*entry{"id": e}, store: lifecycleStore{}, ctx: ctx, cancel: cancel}

		runCtx, cancelRun := context.WithCancel(ctx)
		cancelRun() // user hit pause while the worker was waiting for its slot
		m.abandonRun("id", 1, runCtx)

		e.mu.Lock()
		status := e.dl.Status
		gotCancel := e.cancel
		e.mu.Unlock()
		if status != domain.StatusPaused {
			t.Fatalf("status = %q, want paused", status)
		}
		if gotCancel != nil {
			t.Fatal("abandonRun left a stale cancel function installed")
		}
	})

	t.Run("shutdown leaves queued", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		e := &entry{dl: &domain.Download{ID: "id", Status: domain.StatusQueued}, runID: 1}
		m := &Manager{entries: map[string]*entry{"id": e}, store: lifecycleStore{}, ctx: ctx, cancel: cancel}

		cancel() // process shutdown cancels m.ctx, which cancels runCtx too
		runCtx, cancelRun := context.WithCancel(ctx)
		cancelRun()
		m.abandonRun("id", 1, runCtx)

		e.mu.Lock()
		status := e.dl.Status
		e.mu.Unlock()
		if status != domain.StatusQueued {
			t.Fatalf("status = %q, want queued (so it requeues on next start)", status)
		}
	})
}

// TestReserveNameSkipsExisting pins the O_EXCL reservation: names of
// already-present files are never handed out twice.
func TestReserveNameSkipsExisting(t *testing.T) {
	dir := t.TempDir()
	for i, want := range []string{"file.bin", "file (1).bin", "file (2).bin"} {
		got, err := reserveName(dir, "file.bin")
		if err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
		if got != want {
			t.Fatalf("call %d: reserveName = %q, want %q", i+1, got, want)
		}
		if _, err := os.Stat(filepath.Join(dir, got)); err != nil {
			t.Fatalf("call %d: reserved file missing: %v", i+1, err)
		}
	}
}
