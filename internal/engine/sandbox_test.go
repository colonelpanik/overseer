package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"overseer/internal/loop"
	"overseer/internal/sandbox"
)

func TestSandboxSpecNeverExposesTheDatabase(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	task := h.submit(t, "spec check")
	task.WorktreeDir = filepath.Join(t.TempDir(), "wt")

	for _, agentName := range []string{"claude", "codex"} {
		spec := h.eng.sandboxSpec(task, agentName, agentName == "claude")
		for _, m := range spec.Mounts {
			if strings.HasSuffix(m.Src, ".db") {
				t.Errorf("%s: the database is mounted at %s", agentName, m.Src)
			}
			// The data directory holds the database; only the task's own run
			// directory and the schema file may be exposed.
			if m.Src == h.eng.Cfg.DataDir {
				t.Errorf("%s: the whole data dir is mounted", agentName)
			}
		}
	}
}

func TestSandboxSpecMarksAgentConfigOptional(t *testing.T) {
	// A valid but not-yet-used agent installation has no settings.json or
	// config.toml. bubblewrap aborts on a missing required source, so these
	// must be optional or every task fails under the default sandbox mode.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	task := h.submit(t, "optional mounts")
	task.WorktreeDir = filepath.Join(t.TempDir(), "wt")
	task.GitCommonDir = filepath.Join(t.TempDir(), "git")
	task.GitAdminDir = filepath.Join(task.GitCommonDir, "worktrees", task.Slug)

	mustBeOptional := map[string][]string{
		"claude": {
			filepath.Join(home, ".claude", ".credentials.json"),
			filepath.Join(home, ".claude", "settings.json"),
			filepath.Join(home, ".claude", "plugins"),
			filepath.Join(home, ".claude", "skills"),
		},
		"codex": {
			filepath.Join(home, ".codex", "config.toml"),
			filepath.Join(home, ".codex", "packages"),
		},
	}
	for agentName, paths := range mustBeOptional {
		spec := h.eng.sandboxSpec(task, agentName, agentName == "claude")
		byDest := map[string]sandbox.Mount{}
		for _, m := range spec.Mounts {
			byDest[m.Dest] = m
		}
		for _, p := range paths {
			m, ok := byDest[p]
			if !ok {
				t.Errorf("%s: %s is not mounted at all", agentName, p)
				continue
			}
			if !m.Optional {
				t.Errorf("%s: %s is a required mount; a fresh install would fail", agentName, p)
			}
			if m.Write {
				t.Errorf("%s: %s must be read-only", agentName, p)
			}
		}
		// The worktree and the git dirs, by contrast, must stay required:
		// silently skipping them would run the agent in a broken sandbox.
		for _, p := range []string{task.WorktreeDir, task.GitCommonDir} {
			if m, ok := byDest[p]; !ok || m.Optional {
				t.Errorf("%s: %s must be a required mount", agentName, p)
			}
		}
	}
}

func TestSandboxSpecUsesResolvedGitDirsNotRepoPath(t *testing.T) {
	// When the submitted repository is itself a linked worktree, .git is a
	// file and <repo>/.git/worktrees/<slug> does not exist. The spec must use
	// the dirs resolved by rev-parse.
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	task := h.submit(t, "resolved dirs")
	task.RepoPath = "/some/linked/worktree"
	task.WorktreeDir = filepath.Join(t.TempDir(), "wt")
	task.GitCommonDir = "/real/primary/.git"
	task.GitAdminDir = "/real/primary/.git/worktrees/resolved-dirs"

	spec := h.eng.sandboxSpec(task, "claude", true)
	var paths []string
	for _, m := range spec.Mounts {
		paths = append(paths, m.Src)
	}
	for _, want := range []string{task.GitCommonDir, task.GitAdminDir} {
		if !slices.Contains(paths, want) {
			t.Errorf("mounts %v missing resolved dir %q", paths, want)
		}
	}
	if slices.Contains(paths, filepath.Join(task.RepoPath, ".git")) {
		t.Error("the spec derived a git dir from RepoPath instead of using the resolved one")
	}
}

func TestSandboxSpecGivesTheReviewerNoWriteAccess(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	task := h.submit(t, "reviewer is read only")
	task.WorktreeDir = filepath.Join(t.TempDir(), "wt")

	spec := h.eng.sandboxSpec(task, "codex", false)
	for _, m := range spec.Mounts {
		if m.Write && m.Dest == task.WorktreeDir {
			t.Error("codex has write access to the worktree; the reviewer must never write")
		}
	}
}

func TestSandboxSpecNeverMakesTheRealStateDirWritable(t *testing.T) {
	// The escape this closes: if the real state directory is the writable
	// parent, an agent can CREATE a config file that does not exist yet —
	// settings.local.json is absent on a typical install — and it then runs
	// on the next unsandboxed invocation. No mount may have the real
	// directory, or anything under it, as a writable destination.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	task := h.submit(t, "real state dir stays read only")
	task.WorktreeDir = filepath.Join(t.TempDir(), "wt")

	for agentName, realDir := range map[string]string{
		"claude": filepath.Join(home, ".claude"),
		"codex":  filepath.Join(home, ".codex"),
	} {
		spec := h.eng.sandboxSpec(task, agentName, agentName == "claude")

		var stateMounted bool
		for _, m := range spec.Mounts {
			if m.Dest == realDir {
				stateMounted = true
				if !m.Write {
					t.Errorf("%s: state dir must be writable inside so --resume works", agentName)
				}
				if m.Src == realDir {
					t.Errorf("%s: the REAL state dir is the writable source; "+
						"an agent could plant config that survives the task", agentName)
				}
				if !strings.HasPrefix(m.Src, h.eng.Cfg.DataDir) {
					t.Errorf("%s: writable state source %q is outside overseer's data dir",
						agentName, m.Src)
				}
			}
			// Nothing whose source lies under the real state dir may be writable.
			if m.Write && strings.HasPrefix(m.Src, realDir+string(filepath.Separator)) {
				t.Errorf("%s: %s is mounted writable from the real state dir", agentName, m.Src)
			}
		}
		if !stateMounted {
			t.Errorf("%s: no state dir mounted at %s", agentName, realDir)
		}
	}
}

func TestSandboxSpecGivesClaudeJSONAPerTaskCopy(t *testing.T) {
	// ~/.claude.json carries top-level mcpServers, which is executable
	// configuration. Mounting the real file writable would let an agent add
	// an MCP server that runs on the next unsandboxed invocation.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	task := h.submit(t, "claude.json is copied")
	task.WorktreeDir = filepath.Join(t.TempDir(), "wt")

	realFile := filepath.Join(home, ".claude.json")
	var found bool
	for _, m := range h.eng.sandboxSpec(task, "claude", true).Mounts {
		if m.Dest != realFile {
			continue
		}
		found = true
		if m.Src == realFile {
			t.Error("the real ~/.claude.json is mounted; mcpServers changes would persist")
		}
		if !m.Write {
			t.Error("the copy must be writable, or Claude cannot record project state")
		}
	}
	if !found {
		t.Errorf("nothing mounted at %s", realFile)
	}
}

func TestPrepareAgentStateSeedsAWritableClaudeJSONCopy(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	task := h.submit(t, "seed state")

	if err := h.eng.prepareAgentState(task, "claude"); err != nil {
		t.Fatalf("prepareAgentState: %v", err)
	}
	if fi, err := os.Stat(h.eng.agentStateDir(task, "claude")); err != nil || !fi.IsDir() {
		t.Errorf("state dir not created: %v", err)
	}

	copyPath := h.eng.agentStateFile(task, "claude.json")
	raw, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatalf("claude.json copy not seeded: %v", err)
	}
	if !json.Valid(raw) {
		t.Errorf("seeded claude.json is not valid JSON: %q", raw[:min(80, len(raw))])
	}

	// Idempotent: a second call must not clobber what the agent has written.
	if err := os.WriteFile(copyPath, []byte(`{"marker":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.prepareAgentState(task, "claude"); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(again), "marker") {
		t.Error("prepareAgentState overwrote the task's existing state")
	}
}

func TestSandboxSpecMountsToolchainCaches(t *testing.T) {
	// Without these, a $HOME tmpfs hides GOCACHE and GOMODCACHE and every
	// agent turn rebuilds from cold and re-downloads every dependency.
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	task := h.submit(t, "caches are mounted")
	task.WorktreeDir = filepath.Join(t.TempDir(), "wt")

	spec := h.eng.sandboxSpec(task, "claude", true)
	var gotBuild, gotMod bool
	for _, m := range spec.Mounts {
		if strings.HasSuffix(m.Src, "go-build") {
			gotBuild = true
			if !m.Write {
				t.Error("the build cache must be writable to be of any use")
			}
			if !m.Optional {
				t.Error("a cache path must be optional; not every host has one")
			}
		}
		if strings.HasSuffix(m.Src, filepath.Join("pkg", "mod")) {
			gotMod = true
		}
		if strings.Contains(m.Src, "$HOME") {
			t.Errorf("mount source %q was not expanded", m.Src)
		}
	}
	if !gotBuild || !gotMod {
		t.Errorf("cache mounts missing: build=%v mod=%v", gotBuild, gotMod)
	}
}

// TestSandboxSpecUsesOverseersOwnGoCachesNotTheOperatorsReal is the direct
// regression test for Finding F8: the operator's real ~/.cache/go-build and
// ~/go/pkg/mod must never be mounted writable, since Go's build cache holds
// trusted output blobs that a later *unsandboxed* build would reuse without
// full re-verification. GOCACHE/GOMODCACHE must instead point at a directory
// under overseer's own data dir.
func TestSandboxSpecUsesOverseersOwnGoCachesNotTheOperatorsReal(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	task := h.submit(t, "go cache is overseer owned")
	task.WorktreeDir = filepath.Join(t.TempDir(), "wt")

	spec := h.eng.sandboxSpec(task, "claude", true)

	realGoBuild := filepath.Join(home, ".cache", "go-build")
	realGoMod := filepath.Join(home, "go", "pkg", "mod")
	for _, m := range spec.Mounts {
		if m.Src == realGoBuild || m.Src == realGoMod {
			t.Errorf("the operator's real Go cache is mounted: %+v", m)
		}
	}

	if spec.Env["GOCACHE"] == "" || spec.Env["GOMODCACHE"] == "" {
		t.Fatalf("GOCACHE/GOMODCACHE not set: %+v", spec.Env)
	}
	if !strings.HasPrefix(spec.Env["GOCACHE"], h.eng.Cfg.DataDir) {
		t.Errorf("GOCACHE = %q, want it under the data dir %q", spec.Env["GOCACHE"], h.eng.Cfg.DataDir)
	}
	if !strings.HasPrefix(spec.Env["GOMODCACHE"], h.eng.Cfg.DataDir) {
		t.Errorf("GOMODCACHE = %q, want it under the data dir %q", spec.Env["GOMODCACHE"], h.eng.Cfg.DataDir)
	}

	// The overseer-owned cache dirs must actually be mounted (writable), or
	// pointing GOCACHE/GOMODCACHE at them is pointing into thin air.
	var mountedBuild, mountedMod bool
	for _, m := range spec.Mounts {
		if m.Src == spec.Env["GOCACHE"] {
			mountedBuild = m.Write
		}
		if m.Src == spec.Env["GOMODCACHE"] {
			mountedMod = m.Write
		}
	}
	if !mountedBuild || !mountedMod {
		t.Errorf("overseer's own Go cache dirs are not mounted writable: build=%v mod=%v", mountedBuild, mountedMod)
	}
}

func TestSandboxSpecDoesNotMountCredentialsByDefault(t *testing.T) {
	// Mounting ~/.netrc or an SSH agent socket would hand the agent
	// credentials. Operators can opt in with sandbox_extra_read_only; the
	// default must not.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	task := h.submit(t, "no credentials by default")
	task.WorktreeDir = filepath.Join(t.TempDir(), "wt")

	forbidden := []string{
		filepath.Join(home, ".netrc"),
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".gitconfig"),
		filepath.Join(home, ".aws"),
	}
	for _, m := range h.eng.sandboxSpec(task, "claude", true).Mounts {
		for _, f := range forbidden {
			if m.Src == f {
				t.Errorf("%s is mounted by default", f)
			}
		}
	}
}

func TestTaskRunsToCompletionInsideTheSandbox(t *testing.T) {
	if err := sandbox.Probe("bwrap"); err != nil {
		t.Skipf("bwrap unusable: %v", err)
	}

	// The fake agents are scripts under a temp dir, which the sandbox will
	// not expose — so point ClaudeBin/CodexBin at them and let BinMounts
	// bring their directory in, exactly as it does for the real CLIs.
	claude := fakeClaude(t, `echo 'package main' > added.go`)
	codex := fakeCodex(t, `{"verdict":"approved","findings":[]}`)
	h := newHarness(t, claude, codex)

	wrapper, _, err := sandbox.Select("bwrap", "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	h.eng.Sandbox = wrapper

	ctx := context.Background()
	task := h.submit(t, "runs sandboxed")
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateDone) {
		t.Fatalf("State = %q (err %q), want done; the sandbox broke the loop",
			got.State, got.ErrMsg)
	}
}

// TestSandboxEnvAllowlistIsEnforced is the end-to-end regression test for
// Finding F2, built against the real sandbox.Bwrap and the actual Spec the
// engine assembles — not a mock — exactly as the finding's test requirement
// asks for.
func TestSandboxEnvAllowlistIsEnforced(t *testing.T) {
	if err := sandbox.Probe("bwrap"); err != nil {
		t.Skipf("bwrap unusable: %v", err)
	}

	t.Setenv("OVERSEER_TEST_OUTSIDE_ALLOWLIST", "should-not-leak")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-allowlisted")
	t.Setenv("OVERSEER_TEST_PASSTHROUGH_ONLY", "reaches-via-config")

	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	h.eng.Cfg.SandboxEnvPassthrough = []string{"OVERSEER_TEST_PASSTHROUGH_ONLY"}

	ctx := context.Background()
	task := h.submit(t, "env allowlist check")
	wt, err := h.eng.WT.Create(ctx, h.repo, task.Slug)
	if err != nil {
		t.Fatal(err)
	}
	task.WorktreeDir, task.Branch, task.BaseRef = wt.Dir, wt.Branch, wt.BaseRef
	task.GitCommonDir, task.GitAdminDir = wt.CommonDir, wt.AdminDir

	// Mirrors what runAgent does before wrapping for real: the required
	// mounts (run dir, per-task agent state dir) must exist or bubblewrap
	// aborts outright rather than skipping them.
	if err := sandbox.EnsureDirs(h.eng.runDir(task)); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.prepareAgentState(task, "claude"); err != nil {
		t.Fatal(err)
	}

	spec := h.eng.sandboxSpec(task, "claude", true)
	wrapper, _, err := sandbox.Select("bwrap", "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	bin, args := wrapper.Wrap("/bin/sh",
		[]string{"-c", `echo "[$OVERSEER_TEST_OUTSIDE_ALLOWLIST][$ANTHROPIC_API_KEY][$OVERSEER_TEST_PASSTHROUGH_ONLY]"`},
		spec)
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("sandboxed echo failed: %v: %s", err, out)
	}
	got := string(out)

	if strings.Contains(got, "should-not-leak") {
		t.Errorf("a variable outside the allowlist reached the sandbox: %q", got)
	}
	if !strings.Contains(got, "sk-ant-test-allowlisted") {
		t.Errorf("an allowlisted credential variable (ANTHROPIC_API_KEY) did not reach the sandbox: %q", got)
	}
	if !strings.Contains(got, "reaches-via-config") {
		t.Errorf("a sandbox_env_passthrough entry was not honoured: %q", got)
	}
}

func TestSandboxedAgentCannotReadOutsideItsMounts(t *testing.T) {
	if err := sandbox.Probe("bwrap"); err != nil {
		t.Skipf("bwrap unusable: %v", err)
	}

	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}

	// An agent that tries to exfiltrate the secret into its own output.
	claude := writeScript(t, "claude", `
echo '{"type":"system","subtype":"init","session_id":"claude-sess"}'
echo '# plan' > PLAN.md
if cat `+secret+` > stolen.txt 2>/dev/null; then echo LEAKED > leaked.txt; fi
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"claude-sess","total_cost_usd":0,"usage":{"input_tokens":1,"output_tokens":1}}'
`)
	h := newHarness(t, claude, fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	wrapper, _, err := sandbox.Select("bwrap", "bwrap")
	if err != nil {
		t.Fatal(err)
	}
	h.eng.Sandbox = wrapper

	ctx := context.Background()
	task := h.submit(t, "cannot read secrets")
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Whatever the task's outcome, the secret must not have been read. Check
	// the branch, since the worktree is removed on success.
	out, err := exec.Command("git", "-C", h.repo, "grep", "-l", "PRIVATE KEY",
		got.Branch).CombinedOutput()
	if err == nil && len(out) > 0 {
		t.Errorf("the secret was exfiltrated into the branch: %s", out)
	}
	out, _ = exec.Command("git", "-C", h.repo, "ls-tree", "-r", "--name-only",
		got.Branch).CombinedOutput()
	if strings.Contains(string(out), "leaked.txt") {
		t.Error("the agent successfully read a file outside its mounts")
	}
}
