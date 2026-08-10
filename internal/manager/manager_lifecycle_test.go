package manager

import (
	"context"
	"testing"
)

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
