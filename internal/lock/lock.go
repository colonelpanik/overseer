// Package lock keeps two overseer daemons from ever sharing one data
// directory.
//
// Without it, a second `overseer serve` against the same data dir has each
// process's Recover sweep the other's live steps to "interrupted", both
// claim the same task, and worktree.Create's adopt() then hands the same
// in-use worktree to two `bypassPermissions` agents writing the same files
// concurrently — and both push.
package lock

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// FileLock is an exclusive lock held for the lifetime of this process.
type FileLock struct {
	f *os.File
}

// Acquire takes an exclusive lock on <dataDir>/overseer.lock, failing with a
// clear message naming dataDir if another process already holds it.
//
// This uses flock(2) rather than an O_EXCL pidfile. A pidfile left behind by
// a killed daemon has to be told apart from one still live, which means
// opening it, reading back a PID, and checking whether that process — or,
// worse, an unrelated process that has since reused the PID — is still
// running; every one of those steps is a chance to get "stale" wrong in
// either direction. flock locks are held by the kernel against the open file
// description, not the file's contents, so they are released automatically
// the instant the holding process exits for any reason, including SIGKILL.
// There is no stale-lock case to detect, because there is no stale state for
// the kernel to leave behind: the next Acquire simply succeeds.
//
// The lock file itself is never removed on Release. Unlinking it would race a
// second process that has just opened the same path (open-then-unlink is not
// atomic with the flock), and an empty file left on disk is not something
// that needs cleaning up.
func Acquire(dataDir string) (*FileLock, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("lock: create data dir: %w", err)
	}
	path := filepath.Join(dataDir, "overseer.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock: open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf(
			"another overseer serve is already running against data dir %s (lock held on %s): %w",
			dataDir, path, err)
	}
	// Recorded purely for a human inspecting the file by hand; Acquire never
	// reads it back, since the flock itself is what decides ownership.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	return &FileLock{f: f}, nil
}

// Release drops the lock. Safe to call once on a non-nil *FileLock; the lock
// file is left in place for the next Acquire to reuse.
func (l *FileLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
