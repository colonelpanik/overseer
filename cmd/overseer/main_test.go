package main

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// TestAwaitWorkersBlocksUntilBothWorkersFinish proves the actual defect in
// Finding 1: cmdServe's old select returned the instant ctx.Done() fired,
// without waiting for either the web server or the engine loop to actually
// finish their graceful shutdown. That let the deferred stop()/st.Close()
// race a drain still in progress. awaitWorkers must not return before every
// pending worker has reported, even when one of them finishes quickly and
// the other is still draining.
func TestAwaitWorkersBlocksUntilBothWorkersFinish(t *testing.T) {
	done := make(chan error, 2)

	var mu sync.Mutex
	slowWorkerFinished := false

	go func() {
		// The fast worker (e.g. the HTTP server) reports back almost
		// immediately.
		done <- nil
	}()
	go func() {
		// The slow worker (e.g. the engine draining an in-flight RunTask)
		// takes a little longer. The flag is set right before it reports,
		// so it can only be true if awaitWorkers actually waited for it.
		time.Sleep(150 * time.Millisecond)
		mu.Lock()
		slowWorkerFinished = true
		mu.Unlock()
		done <- nil
	}()

	if err := awaitWorkers(done, 2, 2*time.Second); err != nil {
		t.Fatalf("awaitWorkers returned an error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !slowWorkerFinished {
		t.Fatal("awaitWorkers returned before the slow worker finished draining; " +
			"shutdown must not proceed until both workers have actually returned")
	}
}

// TestAwaitWorkersReturnsTheReportedError checks that a real error from
// either worker still surfaces once both have been accounted for, matching
// cmdServe's existing behaviour of propagating a worker's error to main.
func TestAwaitWorkersReturnsTheReportedError(t *testing.T) {
	done := make(chan error, 2)
	want := errors.New("listen tcp: address already in use")
	done <- want
	done <- nil

	err := awaitWorkers(done, 2, time.Second)
	if !errors.Is(err, want) && (err == nil || err.Error() != want.Error()) {
		t.Fatalf("awaitWorkers = %v, want %v", err, want)
	}
}

// TestAwaitWorkersAbandonsAfterGracePeriod proves the bounded side of the
// fix: a task may legitimately run up to the 30-minute step timeout, so an
// operator pressing Ctrl-C must not be held hostage by a worker that never
// reports. awaitWorkers must give up and return promptly once its grace
// period elapses, rather than blocking forever.
func TestAwaitWorkersAbandonsAfterGracePeriod(t *testing.T) {
	done := make(chan error) // nothing is ever sent: a permanently stuck worker
	grace := 50 * time.Millisecond

	start := time.Now()
	err := awaitWorkers(done, 1, grace)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error describing the abandoned drain, got nil")
	}
	if elapsed < grace {
		t.Errorf("returned after %v, before the %v grace period elapsed", elapsed, grace)
	}
	// Generous upper bound so this stays reliable under CI scheduling jitter
	// while still proving it did not hang.
	if elapsed > grace+2*time.Second {
		t.Errorf("returned after %v; the %v grace period was not honoured promptly", elapsed, grace)
	}
}

// TestAwaitWorkersNoPendingReturnsImmediately guards the trivial case used
// when the errc-fires-first branch of cmdServe has already consumed one of
// the two results.
func TestAwaitWorkersNoPendingReturnsImmediately(t *testing.T) {
	done := make(chan error)
	if err := awaitWorkers(done, 0, time.Second); err != nil {
		t.Fatalf("awaitWorkers with nothing pending = %v, want nil", err)
	}
}
