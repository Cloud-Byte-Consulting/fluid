package runtime

// Specs for CLO-199 "Run store: run IDs, per-node states, and queryable status".
// Each test name maps to a Gherkin scenario on the work item.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Scenario: Status of an active run
func TestStatus_ActiveRun_ReportsPerNodeStates(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Dir: dir}
	runID := store.NewRunID()

	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	r := &Runner{Dir: dir, Exec: func(ctx context.Context, nr NodeRun) (NodeResult, error) {
		if nr.Node.ID == "c" {
			once.Do(func() { close(started) })
			<-release // hold c in flight
		}
		return NodeResult{Output: []byte(`{}`), Tokens: 10}, nil
	}}

	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), threeNodeDag(), runID)
		done <- err
	}()
	<-started // a and b finished; c is executing

	st, err := store.Status(runID)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "running" {
		t.Fatalf("run state = %q, want running", st.State)
	}
	if st.Nodes["a"].State != "succeeded" || st.Nodes["b"].State != "succeeded" {
		t.Fatalf("a/b should be succeeded: %+v", st.Nodes)
	}
	if st.Nodes["c"].State != "running" {
		t.Fatalf("c should be running: %+v", st.Nodes["c"])
	}
	if st.Nodes["a"].Started.IsZero() || st.Nodes["a"].Finished.IsZero() {
		t.Fatalf("a should have timestamps: %+v", st.Nodes["a"])
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Scenario: Status of a terminal run
func TestStatus_TerminalRun_IdentifiesFailureSafely(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Dir: dir}
	runID := store.NewRunID()
	ex := newCountingExec()
	ex.failing["c"] = true
	r := &Runner{Dir: dir, Exec: ex.run}
	if _, err := r.Run(context.Background(), threeNodeDag(), runID); err == nil {
		t.Fatal("expected run failure")
	}

	st, err := store.Status(runID)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "failed" {
		t.Fatalf("run state = %q, want failed", st.State)
	}
	if st.Nodes["c"].State != "failed" || st.Nodes["c"].Error == "" {
		t.Fatalf("c should be failed with a message: %+v", st.Nodes["c"])
	}
	if st.Nodes["a"].State != "succeeded" {
		t.Fatalf("a should remain succeeded: %+v", st.Nodes["a"])
	}
	if st.Finished.IsZero() {
		t.Fatal("terminal run must have a finish time")
	}
}

// Scenario: Unknown run
func TestStatus_UnknownRun_ReturnsNotFound(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	_, err := store.Status("nope")
	if err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("want not_found error, got %v", err)
	}
}

// Scenario: List runs
func TestList_ReturnsSummariesNewestFirst(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Dir: dir}
	ex := newCountingExec()
	r := &Runner{Dir: dir, Exec: ex.run}

	first := store.NewRunID()
	if _, err := r.Run(context.Background(), threeNodeDag(), first); err != nil {
		t.Fatal(err)
	}
	second := store.NewRunID()
	if _, err := r.Run(context.Background(), threeNodeDag(), second); err != nil {
		t.Fatal(err)
	}

	runs, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("want 2 runs, got %d", len(runs))
	}
	if runs[0].RunID != second || runs[1].RunID != first {
		t.Fatalf("want newest first [%s %s], got %+v", second, first, runs)
	}
	if runs[0].State != "succeeded" || runs[0].NodesDone != 3 || runs[0].NodesTotal != 3 {
		t.Fatalf("bad summary: %+v", runs[0])
	}
}

// Scenario (Run store): run ids are unique and filesystem-safe
func TestNewRunID_UniqueAndSafe(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := store.NewRunID()
		if seen[id] {
			t.Fatalf("duplicate run id %s", id)
		}
		seen[id] = true
		if strings.ContainsAny(id, "/\\ ") {
			t.Fatalf("unsafe run id %q", id)
		}
	}
}

// Scenario (CLO-208): prune removes old terminal runs, never active ones
func TestPrune_RemovesOldTerminalRunsOnly(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Dir: dir}
	ex := newCountingExec()
	r := &Runner{Dir: dir, Exec: ex.run}

	oldDone := store.NewRunID()
	if _, err := r.Run(context.Background(), threeNodeDag(), oldDone); err != nil {
		t.Fatal(err)
	}
	freshDone := store.NewRunID()
	if _, err := r.Run(context.Background(), threeNodeDag(), freshDone); err != nil {
		t.Fatal(err)
	}
	oldRunning := store.NewRunID()
	exFail := newCountingExec()
	exFail.failing["a"] = true
	exFail.failing["b"] = true
	rf := &Runner{Dir: dir, Exec: exFail.run}
	rf.Run(context.Background(), threeNodeDag(), oldRunning) // failed = terminal... make it non-terminal instead
	// Overwrite: craft a journal with no terminal event (simulates crash mid-run).
	if err := os.WriteFile(filepath.Join(dir, oldRunning+".jsonl"),
		[]byte(`{"t":"2020-01-01T00:00:00Z","event":"run_started","total":3}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	past := time.Now().Add(-48 * time.Hour)
	for _, id := range []string{oldDone, oldRunning} {
		if err := os.Chtimes(filepath.Join(dir, id+".jsonl"), past, past); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := store.Prune(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != oldDone {
		t.Fatalf("removed = %v, want only %s", removed, oldDone)
	}
	if _, err := store.Status(freshDone); err != nil {
		t.Fatal("fresh run must survive prune")
	}
	if _, err := store.Status(oldRunning); err != nil {
		t.Fatal("non-terminal run must survive prune regardless of age")
	}
}

// Deep-review finding: one corrupt journal must not break listing.
func TestList_SurfacesCorruptJournalsWithoutFailing(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Dir: dir}
	ex := newCountingExec()
	r := &Runner{Dir: dir, Exec: ex.run}
	good := store.NewRunID()
	if _, err := r.Run(context.Background(), threeNodeDag(), good); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run-bad.jsonl"), []byte("\x00garbage\n{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runs, err := store.List()
	if err != nil {
		t.Fatalf("corrupt journal must not fail List: %v", err)
	}
	states := map[string]string{}
	for _, r := range runs {
		states[r.RunID] = r.State
	}
	if states[good] != "succeeded" {
		t.Fatalf("good run lost: %v", states)
	}
	if states["run-bad"] != "corrupt" {
		t.Fatalf("corrupt run must be visible as corrupt: %v", states)
	}
}

// Deep-review finding: a missing state dir is an empty store, not an error.
func TestList_MissingDirIsEmpty(t *testing.T) {
	store := &Store{Dir: filepath.Join(t.TempDir(), "never-created")}
	runs, err := store.List()
	if err != nil || len(runs) != 0 {
		t.Fatalf("want empty, got %v / %v", runs, err)
	}
}
