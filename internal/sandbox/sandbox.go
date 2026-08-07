// Package sandbox confines the agent subprocesses.
//
// It exists because --permission-mode bypassPermissions does not confine
// anything: it skips the permission system rather than narrowing it, so
// without an OS-level sandbox an agent runs with the daemon user's full
// filesystem access. The task worktree isolates concurrent tasks from each
// other; only this package limits what a misbehaving agent can reach.
package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Mount is one path exposed inside the sandbox.
type Mount struct {
	// Src is the path on the host.
	Src string
	// Dest is where it appears inside. Usually equal to Src; they differ when
	// a per-task directory stands in for a real one, which is how the agent
	// gets a writable state directory without the real one being writable.
	Dest  string
	Write bool
	// Optional mounts are skipped when the source does not exist. bubblewrap
	// aborts on a missing --bind source, so a fresh agent install with no
	// settings.json or config.toml would otherwise fail every task.
	Optional bool
}

// Spec describes one sandboxed invocation.
type Spec struct {
	// HomeDir is replaced with an empty tmpfs, then selectively re-exposed
	// by Mounts.
	HomeDir string
	WorkDir string
	// Mounts are applied in order. Order matters: a read-only mount nested
	// inside a read-write one must come after it to take effect.
	Mounts []Mount
	// PathEnv is the PATH the agent sees.
	PathEnv string
}

// Add appends a required mount, returning the updated Spec so calls chain.
// A missing source is a hard failure: if the worktree is not there, running
// the agent anyway would be worse than not starting.
func (s Spec) Add(path string, write bool) Spec {
	return s.AddAt(path, path, write)
}

// AddAt appends a required mount that appears at dest inside. Use it to put a
// per-task directory where the agent expects its real state directory, so the
// real one need never be writable.
func (s Spec) AddAt(src, dest string, write bool) Spec {
	if src == "" || dest == "" {
		return s
	}
	s.Mounts = append(s.Mounts, Mount{Src: src, Dest: dest, Write: write})
	return s
}

// AddOptional appends a mount that is skipped when its source is absent.
// Use it for paths a valid installation may simply not have yet, such as an
// agent's settings file or plugin directory before first use.
func (s Spec) AddOptional(path string, write bool) Spec {
	if path == "" {
		return s
	}
	s.Mounts = append(s.Mounts,
		Mount{Src: path, Dest: path, Write: write, Optional: true})
	return s
}

// Wrapper rewrites a command so it runs confined.
type Wrapper interface {
	// Wrap returns the binary and argv to execute instead of bin/args.
	Wrap(bin string, args []string, spec Spec) (string, []string)
	// Name is the mode's name, for logs and the dashboard.
	Name() string
}

// Passthrough runs commands unconfined.
type Passthrough struct{}

// Wrap returns bin and args unchanged.
func (Passthrough) Wrap(bin string, args []string, _ Spec) (string, []string) {
	return bin, args
}

// Name identifies the mode.
func (Passthrough) Name() string { return "off" }

// Probe checks that the bwrap binary exists and can actually create a
// namespace. Existence alone is not enough: unprivileged user namespaces can
// be disabled by sysctl, and on Ubuntu an AppArmor profile governs whether
// bwrap may use them.
func Probe(bin string) error {
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	cmd := exec.Command(resolved, "--ro-bind", "/", "/", "--proc", "/proc",
		"--dev", "/dev", "/bin/true")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox: %s cannot create a namespace: %w: %s",
			resolved, err, out)
	}
	return nil
}

// Select resolves a configured mode into a Wrapper, returning a one-line
// note describing what is active.
//
// An explicit mode that cannot be honoured is an error: an operator who asked
// for a sandbox must not silently get none. "auto" downgrades, but says so in
// terms that are hard to miss.
func Select(mode, bwrapBin string) (Wrapper, string, error) {
	switch mode {
	case "off":
		return Passthrough{}, "sandbox off: agents run UNSANDBOXED with this user's full filesystem access", nil

	case "bwrap":
		if err := Probe(bwrapBin); err != nil {
			return nil, "", err
		}
		return Bwrap{Bin: bwrapBin}, "sandbox: bwrap", nil

	case "auto", "":
		if err := Probe(bwrapBin); err != nil {
			return Passthrough{}, fmt.Sprintf(
				"sandbox unavailable (%v): agents run UNSANDBOXED with this user's full filesystem access", err), nil
		}
		return Bwrap{Bin: bwrapBin}, "sandbox: bwrap (auto)", nil
	}
	return nil, "", fmt.Errorf("sandbox: unknown mode %q, want auto, bwrap or off", mode)
}

// BinMounts returns the read-only mounts needed to make bin executable inside
// the sandbox: the directory holding it, and — because the agent CLIs are
// installed as symlinks into a versioned directory — the directory holding
// the symlink's target.
func BinMounts(bin string) []Mount {
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []Mount
	add := func(dir string) {
		if dir == "" || dir == "/" || seen[dir] {
			return
		}
		seen[dir] = true
		out = append(out, Mount{Src: dir, Dest: dir, Write: false})
	}
	add(filepath.Dir(resolved))
	if target, err := filepath.EvalSymlinks(resolved); err == nil {
		add(filepath.Dir(target))
	}
	return out
}

// EnsureDirs creates the writable directories a Spec requires, so a first run
// against a fresh agent installation does not fail on a missing mount source.
// Only directories overseer owns or the agent's own state directory are
// created; nothing else is brought into existence on the operator's behalf.
func EnsureDirs(paths ...string) error {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			return fmt.Errorf("sandbox: create %s: %w", p, err)
		}
	}
	return nil
}
