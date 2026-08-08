package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPassthroughLeavesArgvAlone(t *testing.T) {
	bin, args := Passthrough{}.Wrap("claude", []string{"-p", "hi"}, Spec{})
	if bin != "claude" {
		t.Errorf("bin = %q, want claude", bin)
	}
	if len(args) != 2 || args[0] != "-p" || args[1] != "hi" {
		t.Errorf("args = %v, want [-p hi]", args)
	}
	if (Passthrough{}).Name() != "off" {
		t.Errorf("Name = %q, want off", (Passthrough{}).Name())
	}
}

func TestSpecAddPreservesOrder(t *testing.T) {
	// Order is load-bearing: a read-only mount nested inside a read-write
	// one must be applied after it, or it is overwritten.
	s := Spec{}.Add("/home/u/.claude", true).Add("/home/u/.claude/settings.json", false)
	if len(s.Mounts) != 2 {
		t.Fatalf("Mounts = %d, want 2", len(s.Mounts))
	}
	if !s.Mounts[0].Write || s.Mounts[1].Write {
		t.Errorf("mount writability wrong: %+v", s.Mounts)
	}
	if s.Mounts[1].Src != "/home/u/.claude/settings.json" {
		t.Errorf("order not preserved: %+v", s.Mounts)
	}
}

func TestAllowedEnvKeepsOnlyTheAllowlistAndExtras(t *testing.T) {
	environ := []string{
		"GITHUB_TOKEN=ghp_leak",
		"AWS_SECRET_ACCESS_KEY=leak",
		"HOME=/home/operator",
		"PATH=/usr/bin",
		"TERM=xterm-256color",
		"LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8",
		"ANTHROPIC_API_KEY=sk-ant",
		"CLAUDE_CODE_FOO=bar",
		"CODEX_HOME=/x",
		"OPENAI_API_KEY=sk-oai",
		"MY_CUSTOM_PROXY_TOKEN=extra",
	}

	got := AllowedEnv(environ, []string{"MY_CUSTOM_PROXY_TOKEN"})

	for _, wantAbsent := range []string{"GITHUB_TOKEN", "AWS_SECRET_ACCESS_KEY", "HOME", "PATH"} {
		if _, ok := got[wantAbsent]; ok {
			t.Errorf("%s must not reach the sandbox, got %q", wantAbsent, got[wantAbsent])
		}
	}
	for key, want := range map[string]string{
		"TERM":                  "xterm-256color",
		"LANG":                  "en_US.UTF-8",
		"LC_ALL":                "en_US.UTF-8",
		"ANTHROPIC_API_KEY":     "sk-ant",
		"CLAUDE_CODE_FOO":       "bar",
		"CODEX_HOME":            "/x",
		"OPENAI_API_KEY":        "sk-oai",
		"MY_CUSTOM_PROXY_TOKEN": "extra",
	} {
		if got[key] != want {
			t.Errorf("AllowedEnv[%s] = %q, want %q", key, got[key], want)
		}
	}
}

func TestAllowedEnvWithNoExtrasStillKeepsTheFixedAllowlist(t *testing.T) {
	got := AllowedEnv([]string{"TERM=xterm", "SOME_RANDOM_VAR=x"}, nil)
	if got["TERM"] != "xterm" {
		t.Errorf("TERM missing from a default allowlist: %+v", got)
	}
	if _, ok := got["SOME_RANDOM_VAR"]; ok {
		t.Error("an arbitrary var leaked in with no passthrough configured")
	}
}

func TestBwrapArgvShape(t *testing.T) {
	spec := Spec{
		HomeDir: "/home/u",
		WorkDir: "/wt",
		PathEnv: "/home/u/.local/bin:/usr/bin",
	}.Add("/wt", true).Add("/repo/.git", false)

	bin, args := Bwrap{Bin: "bwrap"}.Wrap("claude", []string{"-p", "go"}, spec)
	if bin != "bwrap" {
		t.Fatalf("bin = %q, want bwrap", bin)
	}
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"--ro-bind /usr /usr",
		"--proc /proc",
		"--dev /dev",
		"--tmpfs /tmp",
		"--tmpfs /home/u",
		"--ro-bind-try /run/systemd/resolve /run/systemd/resolve",
		"--bind /wt /wt",
		"--ro-bind /repo/.git /repo/.git",
		"--setenv HOME /home/u",
		"--chdir /wt",
		"--die-with-parent",
		"--unshare-pid",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q\ngot: %s", want, joined)
		}
	}

	// The tmpfs over $HOME must precede the mounts that re-expose parts of
	// it, otherwise those mounts are hidden.
	if strings.Index(joined, "--tmpfs /home/u") > strings.Index(joined, "--bind /wt /wt") {
		t.Error("--tmpfs $HOME must come before the individual mounts")
	}

	// The command must be last, after a -- separator.
	sep := slices.Index(args, "--")
	if sep < 0 {
		t.Fatal("argv has no -- separator before the command")
	}
	tail := args[sep+1:]
	if len(tail) != 3 || tail[0] != "claude" || tail[1] != "-p" || tail[2] != "go" {
		t.Errorf("command tail = %v, want [claude -p go]", tail)
	}

	// Network must NOT be unshared: the agents call an HTTP API.
	if slices.Contains(args, "--unshare-net") {
		t.Error("--unshare-net would break the agents' API calls")
	}
}

func TestSelectModes(t *testing.T) {
	// "off" always yields a passthrough.
	w, note, err := Select("off", "bwrap")
	if err != nil {
		t.Fatalf("Select(off): %v", err)
	}
	if w.Name() != "off" {
		t.Errorf("Name = %q, want off", w.Name())
	}
	if note == "" {
		t.Error("Select must return a note explaining the active mode")
	}

	// An explicit mode with a missing binary is an error, not a silent
	// downgrade: the operator asked for a sandbox.
	if _, _, err := Select("bwrap", filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("Select(bwrap) with a missing binary must fail loudly")
	}

	// auto with a missing binary downgrades and says so.
	w, note, err = Select("auto", filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("Select(auto): %v", err)
	}
	if w.Name() != "off" {
		t.Errorf("auto with no bwrap: Name = %q, want off", w.Name())
	}
	if !strings.Contains(strings.ToLower(note), "unsandboxed") {
		t.Errorf("note = %q; it must make the downgrade obvious", note)
	}

	if _, _, err := Select("nonsense", "bwrap"); err == nil {
		t.Error("an unknown mode must be an error")
	}
}

func TestBinMountsCoversSymlinkTarget(t *testing.T) {
	// `claude` is ~/.local/bin/claude symlinked into
	// ~/.local/share/claude/versions/<v>. Mounting only the symlink's
	// directory would leave the real executable missing.
	dir := t.TempDir()
	realDir := filepath.Join(dir, "versions")
	linkDir := filepath.Join(dir, "bin")
	for _, d := range []string{realDir, linkDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	realBin := filepath.Join(realDir, "2.1.223")
	if err := os.WriteFile(realBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "claude")
	if err := os.Symlink(realBin, link); err != nil {
		t.Fatal(err)
	}

	mounts := BinMounts(link)
	var paths []string
	for _, m := range mounts {
		paths = append(paths, m.Src)
		if m.Write {
			t.Errorf("binary mount %s must be read-only", m.Src)
		}
	}
	for _, want := range []string{linkDir, realDir} {
		if !slices.Contains(paths, want) {
			t.Errorf("BinMounts = %v, missing %q", paths, want)
		}
	}
}

func TestBinMountsOnMissingBinaryReturnsNothing(t *testing.T) {
	if got := BinMounts(filepath.Join(t.TempDir(), "nope")); len(got) != 0 {
		t.Errorf("BinMounts = %v, want empty for a missing binary", got)
	}
}

// The failure this exists to name: the outer sandbox works, the agent's own
// one inside it does not, and the agent's error reads like ours breaking.
func TestProbeNestedReportsWhatItActuallyTested(t *testing.T) {
	if err := Probe("bwrap"); err != nil {
		t.Skip("bwrap unusable here; the nested probe has nothing to say")
	}
	err := ProbeNested("bwrap")
	if err == nil {
		return // nesting permitted on this machine, which is also a valid answer
	}
	// When it does fail, it must say it was the *inner* one, or the note is
	// indistinguishable from Probe's and sends the operator after the wrong
	// thing entirely.
	if !strings.Contains(err.Error(), "inside this one") {
		t.Errorf("ProbeNested error = %q, want it to name the nested sandbox", err)
	}
}

func TestProbeNestedOnAMissingBinary(t *testing.T) {
	if err := ProbeNested("definitely-not-a-real-bwrap"); err == nil {
		t.Fatal("want an error for a binary that is not there")
	}
}

// Whatever this kernel allows, Select must still hand back a usable wrapper —
// nesting is the agent's problem, not a reason to refuse to run.
func TestSelectStillWorksWhateverNestingAllows(t *testing.T) {
	if err := Probe("bwrap"); err != nil {
		t.Skip("bwrap unusable here")
	}
	w, note, err := Select("auto", "bwrap")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if w.Name() != "bwrap" {
		t.Errorf("Name = %q, want bwrap", w.Name())
	}
	// When nesting is refused, the note has to say what overseer did about it —
	// that the agents' own sandbox is off — because that is the part an
	// operator needs to know. "nesting refused" alone reads like a fault.
	if ProbeNested("bwrap") != nil && !strings.Contains(note, "agents' own sandbox off") {
		t.Errorf("note = %q, want it to say the agents' own sandbox is off", note)
	}
}
