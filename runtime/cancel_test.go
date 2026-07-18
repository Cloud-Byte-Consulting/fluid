package runtime

// Specs for CLO-201 "Cancellation and clean shutdown".

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Scenario: Cancel mid-wave
func TestCancel_MidWaveStopsDispatchAndKeepsJournal(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Dir: dir}
	runID := store.NewRunID()

	inA := make(chan struct{})
	var once sync.Once
	ex := newCountingExec()
	r := &Runner{Dir: dir, Exec: func(ctx context.Context, nr NodeRun) (NodeResult, error) {
		if nr.Node.ID == "a" || nr.Node.ID == "b" {
			once.Do(func() { close(inA) })
			<-ctx.Done() // in-flight nodes observe cancellation
			return NodeResult{}, ctx.Err()
		}
		return ex.run(ctx, nr)
	}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, threeNodeDag(), runID)
		done <- err
	}()
	<-inA
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop on cancel")
	}
	if ex.counts["c"] != 0 {
		t.Fatal("c dispatched after cancellation")
	}
	st, err := store.Status(runID)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "cancelled" {
		t.Fatalf("run state = %q, want cancelled", st.State)
	}
}

// Scenario: Resume a cancelled run
func TestCancel_CancelledRunResumes(t *testing.T) {
	dir := t.TempDir()
	ex := newCountingExec()

	// First attempt: cancel after wave 1 completes, before c runs.
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{Dir: dir, Exec: func(c2 context.Context, nr NodeRun) (NodeResult, error) {
		res, err := ex.run(c2, nr)
		// After the first wave completes, cancel before wave 2 dispatches.
		ex.mu.Lock()
		bothDone := ex.counts["a"] >= 1 && ex.counts["b"] >= 1
		ex.mu.Unlock()
		if bothDone {
			cancel()
		}
		return res, err
	}}
	if _, err := r.Run(ctx, threeNodeDag(), "run-c-resume"); err == nil {
		t.Fatal("expected cancellation")
	}
	if ex.counts["c"] != 0 {
		t.Fatal("c should not have run")
	}

	// Resume with a fresh context: only c executes.
	r2 := &Runner{Dir: dir, Exec: ex.run}
	if _, err := r2.Run(context.Background(), threeNodeDag(), "run-c-resume"); err != nil {
		t.Fatal(err)
	}
	if ex.counts["a"] != 1 || ex.counts["b"] != 1 || ex.counts["c"] != 1 {
		t.Fatalf("resume repeated work: %+v", ex.counts)
	}
}

// Scenario: Journal integrity under interruption
func TestJournal_ToleratesTruncatedFinalLine(t *testing.T) {
	dir := t.TempDir()
	ex := newCountingExec()
	r := &Runner{Dir: dir, Exec: ex.run}
	if _, err := r.Run(context.Background(), threeNodeDag(), "run-trunc"); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-write: append half a JSON line.
	path := filepath.Join(dir, "run-trunc.jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"event":"node_do`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	store := &Store{Dir: dir}
	st, err := store.Status("run-trunc")
	if err != nil {
		t.Fatalf("truncated final line must be tolerated: %v", err)
	}
	if st.State != "succeeded" {
		t.Fatalf("state = %q, want succeeded", st.State)
	}
}

// Scenario: Journal integrity — corruption that is NOT a trailing partial line is an error
func TestJournal_RejectsMidFileCorruption(t *testing.T) {
	dir := t.TempDir()
	ex := newCountingExec()
	r := &Runner{Dir: dir, Exec: ex.run}
	if _, err := r.Run(context.Background(), threeNodeDag(), "run-corrupt"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "run-corrupt.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt a line in the middle, keeping the trailing newline structure.
	data[10] = 0x00
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store := &Store{Dir: dir}
	if _, err := store.Status("run-corrupt"); err == nil {
		t.Fatal("mid-file corruption must surface as an error")
	}
}
