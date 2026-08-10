package manager

import (
	"context"
	"testing"

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
