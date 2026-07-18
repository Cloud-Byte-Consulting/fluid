package runtime

// Specs for CLO-200 "Node retry policy with error classification" and
// CLO-204 "Token budget tracking and enforcement".

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedExec fails node "c" with err for the first n attempts, then succeeds.
type scriptedExec struct {
	mu       sync.Mutex
	attempts map[string]int
	failFor  int
	err      error
	tokens   int
}

func (s *scriptedExec) run(_ context.Context, nr NodeRun) (NodeResult, error) {
	s.mu.Lock()
	s.attempts[nr.Node.ID]++
	n := s.attempts[nr.Node.ID]
	s.mu.Unlock()
	if nr.Node.ID == "c" && n <= s.failFor {
		return NodeResult{}, s.err
	}
	return NodeResult{Output: []byte(`{}`), Tokens: s.tokens}, nil
}

func noBackoff(int) time.Duration { return 0 }

// Scenario: Retry a transient failure
func TestRetry_TransientFailureSucceedsWithinMaxRounds(t *testing.T) {
	ex := &scriptedExec{attempts: map[string]int{}, failFor: 2, err: Transient(errors.New("rate limited"))}
	r := &Runner{Dir: t.TempDir(), Exec: ex.run, Backoff: noBackoff}
	d := threeNodeDag() // MaxRounds: 3
	if _, err := r.Run(context.Background(), d, "run-retry"); err != nil {
		t.Fatalf("expected success on third attempt: %v", err)
	}
	if ex.attempts["c"] != 3 {
		t.Fatalf("c attempts = %d, want 3", ex.attempts["c"])
	}
}

// Scenario: Fail fast on a permanent error
func TestRetry_PermanentErrorFailsAfterOneAttempt(t *testing.T) {
	ex := &scriptedExec{attempts: map[string]int{}, failFor: 99, err: errors.New("bad request")}
	r := &Runner{Dir: t.TempDir(), Exec: ex.run, Backoff: noBackoff}
	if _, err := r.Run(context.Background(), threeNodeDag(), "run-perm"); err == nil {
		t.Fatal("expected failure")
	}
	if ex.attempts["c"] != 1 {
		t.Fatalf("c attempts = %d, want 1 (fail fast)", ex.attempts["c"])
	}
}

// Scenario: Exhaust the round cap
func TestRetry_ExhaustsMaxRounds(t *testing.T) {
	ex := &scriptedExec{attempts: map[string]int{}, failFor: 99, err: Transient(errors.New("always down"))}
	r := &Runner{Dir: t.TempDir(), Exec: ex.run, Backoff: noBackoff}
	d := threeNodeDag()
	d.Caps.MaxRounds = 2
	_, err := r.Run(context.Background(), d, "run-exhaust")
	if err == nil || !strings.Contains(err.Error(), "rounds_exhausted") {
		t.Fatalf("want rounds_exhausted, got %v", err)
	}
	if ex.attempts["c"] != 2 {
		t.Fatalf("c attempts = %d, want 2", ex.attempts["c"])
	}
}

// Scenario: Backoff respects context cancellation
func TestRetry_BackoffStopsOnCancel(t *testing.T) {
	ex := &scriptedExec{attempts: map[string]int{}, failFor: 99, err: Transient(errors.New("down"))}
	r := &Runner{Dir: t.TempDir(), Exec: ex.run, Backoff: func(int) time.Duration { return time.Hour }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, threeNodeDag(), "run-cancel-backoff")
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // let c fail once and enter backoff
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop promptly on cancel during backoff")
	}
}

// --- CLO-204 budget ---

// Scenario: Track usage across nodes / Scenario: Refuse dispatch over budget
func TestBudget_RefusesDispatchWhenExhausted(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Dir: dir}
	runID := store.NewRunID()
	// Each node reports 60 tokens; budget 100. Wave 1 (a,b) spends 120 -> c must not dispatch.
	ex := &scriptedExec{attempts: map[string]int{}, tokens: 60}
	r := &Runner{Dir: dir, Exec: ex.run, Backoff: noBackoff}
	d := threeNodeDag()
	d.Caps.TokenBudget = 100
	_, err := r.Run(context.Background(), d, runID)
	if err == nil || !strings.Contains(err.Error(), "budget_exceeded") {
		t.Fatalf("want budget_exceeded, got %v", err)
	}
	if ex.attempts["c"] != 0 {
		t.Fatalf("c dispatched despite exhausted budget (attempts=%d)", ex.attempts["c"])
	}
	st, err := store.Status(runID)
	if err != nil {
		t.Fatal(err)
	}
	if st.TokensSpent != 120 {
		t.Fatalf("TokensSpent = %d, want 120", st.TokensSpent)
	}
	if st.State != "failed" || !strings.Contains(st.Reason, "budget_exceeded") {
		t.Fatalf("run should fail with budget_exceeded reason: %+v", st)
	}
}

// Scenario: In-flight nodes complete (budget only blocks subsequent dispatch)
func TestBudget_CompletedWorkIsJournaledAndResumable(t *testing.T) {
	dir := t.TempDir()
	ex := &scriptedExec{attempts: map[string]int{}, tokens: 60}
	r := &Runner{Dir: dir, Exec: ex.run, Backoff: noBackoff}
	d := threeNodeDag()
	d.Caps.TokenBudget = 100
	if _, err := r.Run(context.Background(), d, "run-budget-resume"); err == nil {
		t.Fatal("expected budget_exceeded")
	}
	// Raise the budget and resume: a and b must not re-execute.
	d.Caps.TokenBudget = 1000
	if _, err := r.Run(context.Background(), d, "run-budget-resume"); err != nil {
		t.Fatal(err)
	}
	if ex.attempts["a"] != 1 || ex.attempts["b"] != 1 || ex.attempts["c"] != 1 {
		t.Fatalf("resume repeated work: %+v", ex.attempts)
	}
}
