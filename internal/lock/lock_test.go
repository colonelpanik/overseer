package lock

import (
	"syscall"
	"testing"
)

// TestSecondAcquireFailsWhileFirstIsHeld is the direct regression test for
// Finding F5: two overseer daemons must never share one data directory.
// flock's ownership is per open file description, not per process, so two
// independent Acquire calls in one test process genuinely exercise the same
// contention two separate `overseer serve` processes would hit.
func TestSecondAcquireFailsWhileFirstIsHeld(t *testing.T) {
	dir := t.TempDir()

	first, err := Acquire(dir)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer first.Release()

	if _, err := Acquire(dir); err == nil {
		t.Fatal("a second Acquire succeeded while the first lock was held")
	}
}

func TestLockIsReleasedAfterClose(t *testing.T) {
	dir := t.TempDir()

	first, err := Acquire(dir)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	second, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after Release should succeed: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
}

func TestReleaseIsSafeToCallOnANilLock(t *testing.T) {
	var l *FileLock
	if err := l.Release(); err != nil {
		t.Errorf("Release on a nil *FileLock = %v, want nil", err)
	}
}

// TestStaleLockFromAKilledProcessIsReleasedByTheKernel proves the other half
// of F5's design: a daemon killed with no chance to run any cleanup code
// still releases its lock, because the kernel — not this package — is what
// holds it.
//
// A real SIGKILL is simulated at the syscall level rather than by spawning
// and killing a second process: when any process is killed, the kernel
// closes every file descriptor it had open, which is exactly what releases
// an flock, no matter how abruptly the process went away or what userspace
// cleanup code it never got to run. Forcing the raw fd closed here reproduces
// that precisely, and — unlike shelling out to an external lock-holding
// process — does so without the test's own success depending on exactly how
// some other program forks or execs the command it wraps.
func TestStaleLockFromAKilledProcessIsReleasedByTheKernel(t *testing.T) {
	dir := t.TempDir()

	first, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := Acquire(dir); err == nil {
		t.Fatal("a second Acquire succeeded while the first lock was still open")
	}

	if err := syscall.Close(int(first.f.Fd())); err != nil {
		t.Fatalf("force-close the holder's fd: %v", err)
	}

	lk, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after the holder's fd was force-closed "+
			"(the kernel should have released the flock, exactly as it would for a killed process): %v", err)
	}
	if err := lk.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
}
