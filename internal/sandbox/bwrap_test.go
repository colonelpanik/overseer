package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireBwrap skips when a working bwrap is unavailable, so the suite still
// passes on a host that cannot sandbox.
func requireBwrap(t *testing.T) Bwrap {
	t.Helper()
	if err := Probe("bwrap"); err != nil {
		t.Skipf("bwrap unusable: %v", err)
	}
	return Bwrap{Bin: "bwrap"}
}

// runIn executes a shell snippet inside the sandbox and reports its exit
// status.
func runIn(t *testing.T, b Bwrap, spec Spec, script string) (string, bool) {
	t.Helper()
	bin, args := b.Wrap("/bin/sh", []string{"-c", script}, spec)
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

func TestBwrapConfinesTheFilesystem(t *testing.T) {
	b := requireBwrap(t)

	home := t.TempDir() // stands in for $HOME
	worktree := t.TempDir()
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A file inside the fake home that must NOT be visible: the tmpfs hides
	// everything not explicitly mounted.
	if err := os.WriteFile(filepath.Join(home, "dotfile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := Spec{
		HomeDir: home,
		WorkDir: worktree,
		PathEnv: "/usr/bin:/bin",
	}.Add(worktree, true)

	cases := []struct {
		name   string
		script string
		wantOK bool
	}{
		{"write in worktree", "echo x > " + filepath.Join(worktree, "probe"), true},
		{"read the secret", "cat " + secret, false},
		{"list the secret's dir", "ls " + secretDir, false},
		{"read a hidden dotfile in $HOME", "cat " + filepath.Join(home, "dotfile"), false},
		{"run a system binary", "/bin/true", true},
	}
	for _, c := range cases {
		out, ok := runIn(t, b, spec, c.script)
		if ok != c.wantOK {
			t.Errorf("%s: ok = %v, want %v\noutput: %s", c.name, ok, c.wantOK, out)
		}
	}

	// The write must have reached the real directory, not an ephemeral tmpfs.
	if _, err := os.Stat(filepath.Join(worktree, "probe")); err != nil {
		t.Errorf("worktree write did not persist outside the sandbox: %v", err)
	}
}

func TestBwrapInvertedStateLayeringBlocksPlantedConfig(t *testing.T) {
	b := requireBwrap(t)

	home := t.TempDir()
	realState := filepath.Join(home, ".agent")
	if err := os.MkdirAll(realState, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(realState, "settings.json")
	if err := os.WriteFile(settings, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Deliberately absent: this is the path an agent would plant.
	absent := filepath.Join(realState, "settings.local.json")

	// A per-task directory stands in for the real state dir, with the real
	// config layered back read-only.
	taskState := t.TempDir()
	// WorkDir needs its own mount at its own path too: it is the --chdir
	// target, and AddAt below only re-exposes it at realState's path inside
	// the sandbox, not at its own. Production code does the analogous thing
	// by mounting task.WorktreeDir at its own path separately from any
	// per-task state directory.
	spec := Spec{HomeDir: home, WorkDir: taskState, PathEnv: "/usr/bin:/bin"}.
		Add(taskState, true).
		AddAt(taskState, realState, true).
		AddOptional(settings, false).
		AddOptional(absent, false)

	// Session-style writes must work, or --resume cannot.
	if out, ok := runIn(t, b, spec, "echo x > "+filepath.Join(realState, "session")); !ok {
		t.Errorf("writing session state failed: %s", out)
	}
	// The pinned config must be unwritable.
	if out, ok := runIn(t, b, spec, "echo evil >> "+settings); ok {
		t.Errorf("the read-only config was writable: %s", out)
	}
	// And the absent one may be created inside, but must not escape.
	runIn(t, b, spec, "echo evil > "+absent)

	if got, err := os.ReadFile(settings); err != nil || string(got) != "{}" {
		t.Errorf("settings.json was modified: %q (err %v)", got, err)
	}
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Errorf("a planted config file reached the real state dir (err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(realState, "session")); !os.IsNotExist(err) {
		t.Error("session state leaked into the real state dir")
	}
	// It should be in the per-task directory instead.
	if _, err := os.Stat(filepath.Join(taskState, "session")); err != nil {
		t.Errorf("session state did not land in the per-task dir: %v", err)
	}
}

func TestBwrapKeepsNetworkAndDNS(t *testing.T) {
	b := requireBwrap(t)
	// The agents call an HTTPS API, and /etc/resolv.conf is a symlink into
	// /run on systemd-resolved hosts. Without the resolve bind, every agent
	// call fails to resolve its endpoint.
	spec := Spec{HomeDir: t.TempDir(), WorkDir: "/tmp", PathEnv: "/usr/bin:/bin"}
	if out, ok := runIn(t, b, spec, "getent hosts api.anthropic.com"); !ok {
		t.Errorf("DNS resolution failed inside the sandbox: %s", out)
	}
}

func TestBwrapDiesWithParent(t *testing.T) {
	b := requireBwrap(t)
	spec := Spec{HomeDir: t.TempDir(), WorkDir: "/tmp", PathEnv: "/usr/bin:/bin"}
	bin, args := b.Wrap("/bin/sh", []string{"-c", "exec sleep 60"}, spec)

	cmd := exec.Command(bin, args...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	// --die-with-parent means the inner sleep goes away with bwrap. Without
	// it, the step timeout could leak a running agent.
	out, _ := exec.Command("ps", "-eo", "args=").Output()
	if strings.Contains(string(out), "exec sleep 60") {
		t.Error("the sandboxed child survived its parent being killed")
	}
}
