package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/config"
	"overseer/internal/store"
)

func TestResolveRoleUsesTheConfiguredAgentAndModel(t *testing.T) {
	h := newHarness(t, "true", "true")
	h.eng.Cfg.Providers["inhouse"] = config.Provider{
		Kind: config.KindOpenAI, BaseURL: "https://llm.dc.internal/v1",
		KeyEnv: "OVERSEER_TEST_LLM_KEY", Models: []string{"qwen3-coder-480b"},
	}
	h.eng.Cfg.Roles[config.RoleCode] = config.Role{
		Agent: config.AgentCodex, Provider: "inhouse", Model: "qwen3-coder-480b",
	}
	t.Setenv("OVERSEER_TEST_LLM_KEY", "secret-value")

	r, err := h.eng.resolveRole(config.RoleCode)
	if err != nil {
		t.Fatalf("resolveRole: %v", err)
	}
	if r.Agent != config.AgentCodex || r.Runner != h.eng.Codex {
		t.Errorf("role = %+v, want it to run through codex", r)
	}
	if !r.Writable {
		t.Error("the coder must get a writable worktree whichever CLI it runs through")
	}
	if r.Env["OPENAI_BASE_URL"] != "https://llm.dc.internal/v1" {
		t.Errorf("env = %v, want the endpoint", r.Env)
	}
	if r.Env["OPENAI_API_KEY"] != "secret-value" {
		t.Errorf("env did not carry the key read from OVERSEER_TEST_LLM_KEY")
	}

	// And the argv carries the model.
	args := r.args("prompt", "", "", "")
	if !containsArg(args, "-m") || !containsArg(args, "qwen3-coder-480b") {
		t.Errorf("args = %v, want the model passed to codex", args)
	}
}

func TestResolveRoleReportsAMissingKeyForACustomEndpoint(t *testing.T) {
	// The CLI would report this as an authentication failure, which pauses
	// the whole run and leaves the operator guessing which provider was
	// unconfigured.
	h := newHarness(t, "true", "true")
	h.eng.Cfg.Providers["inhouse"] = config.Provider{
		Kind: config.KindOpenAI, BaseURL: "https://llm.dc.internal/v1",
		KeyEnv: "OVERSEER_TEST_ABSENT_KEY",
	}
	h.eng.Cfg.Roles[config.RoleReview] = config.Role{
		Agent: config.AgentCodex, Provider: "inhouse",
	}

	_, err := h.eng.resolveRole(config.RoleReview)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "OVERSEER_TEST_ABSENT_KEY") {
		t.Errorf("error = %v, want it to name the variable that is not set", err)
	}
}

func TestResolveRoleToleratesAMissingKeyOnTheVendorDefault(t *testing.T) {
	// Against the vendor's own endpoint the CLI may be logged in through its
	// own stored credentials, which is the common case — refusing there would
	// break every existing install.
	h := newHarness(t, "true", "true")
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := h.eng.resolveRole(config.RoleCode); err != nil {
		t.Errorf("resolveRole: %v", err)
	}
}

func TestReviewerNeverGetsAWritableWorktree(t *testing.T) {
	// The property that must survive free assignment: a reviewer cannot edit
	// the diff it was asked to judge, whichever CLI it runs through.
	h := newHarness(t, "true", "true")
	h.eng.Cfg.Roles[config.RoleReview] = config.Role{
		Agent: config.AgentClaude, Provider: "anthropic",
	}
	task := h.submit(t, "spec check")
	task.WorktreeDir = filepath.Join(t.TempDir(), "wt")

	r, err := h.eng.resolveRole(config.RoleReview)
	if err != nil {
		t.Fatal(err)
	}
	if r.Writable {
		t.Fatal("the review role must never be writable")
	}

	spec := h.eng.sandboxSpec(task, r.Agent, r.Writable)
	for _, m := range spec.Mounts {
		if m.Src == task.WorktreeDir && m.Write {
			t.Error("the worktree is mounted writable for a claude reviewer")
		}
	}
}

func TestCoderGetsAWritableWorktreeThroughEitherCLI(t *testing.T) {
	h := newHarness(t, "true", "true")
	task := h.submit(t, "spec check")
	task.WorktreeDir = filepath.Join(t.TempDir(), "wt")

	for _, agentName := range []string{config.AgentClaude, config.AgentCodex} {
		spec := h.eng.sandboxSpec(task, agentName, true)
		var writable bool
		for _, m := range spec.Mounts {
			if m.Src == task.WorktreeDir && m.Write {
				writable = true
			}
		}
		if !writable {
			t.Errorf("%s as the coder did not get a writable worktree", agentName)
		}
	}
}

func TestReviewerWithoutSchemaSupportGetsTheContractInThePrompt(t *testing.T) {
	// `claude -p` has no --output-schema, so the schema becomes prompt text
	// and the strict parser is the only guarantee left. Dropping it silently
	// would leave the reviewer with no stated contract at all.
	h := newHarness(t, "true", "true")
	h.eng.Cfg.Roles[config.RoleReview] = config.Role{
		Agent: config.AgentClaude, Provider: "anthropic",
	}
	r, err := h.eng.resolveRole(config.RoleReview)
	if err != nil {
		t.Fatal(err)
	}
	if r.structured() {
		t.Fatal("a claude reviewer cannot be told to emit a schema-checked object")
	}

	prompt := withInlineSchema("Review the diff.")
	for _, want := range []string{"changes_requested", "findings", "severity"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the inline schema is missing %q", want)
		}
	}

	// The argv must not carry codex-only flags.
	args := r.args(prompt, "", "", "")
	for _, unwanted := range []string{"--output-schema", "--output-last-message"} {
		if containsArg(args, unwanted) {
			t.Errorf("args = %v, want no %s for a claude reviewer", args, unwanted)
		}
	}
}

func TestReviewOutputPrefersTheFileAndFallsBackToTheMessage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "verdict.json")
	if err := os.WriteFile(path, []byte(`{"from":"file"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := string(reviewOutput(path, `{"from":"message"}`)); got != `{"from":"file"}` {
		t.Errorf("got %s, want the file", got)
	}
	// No path at all: a reviewer whose CLI cannot write one.
	if got := string(reviewOutput("", `{"from":"message"}`)); got != `{"from":"message"}` {
		t.Errorf("got %s, want the final message", got)
	}
	// A path that was never written: the CLI failed to produce it.
	missing := filepath.Join(dir, "nope.json")
	if got := string(reviewOutput(missing, `{"from":"message"}`)); got != `{"from":"message"}` {
		t.Errorf("got %s, want the final message", got)
	}
}

func TestStepsRecordTheRoleNotTheCLI(t *testing.T) {
	// The dashboard's two lanes, the convergence chart and the oscillation
	// fingerprint all key off this value. Recording the CLI would put a
	// claude reviewer in the coder's lane.
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task := h.submit(t, "Record the role")
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	steps, err := h.st.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) == 0 {
		t.Fatal("no steps recorded")
	}
	for _, s := range steps {
		switch s.Agent {
		case config.RoleCode, config.RoleReview, "verify":
		default:
			t.Errorf("step agent = %q, want a role name", s.Agent)
		}
	}
}

func TestAnalysisRunsThroughItsConfiguredAgent(t *testing.T) {
	h := newHarness(t, fakeAnalyst(t, twoTaskProposal), "true")
	r, err := h.eng.resolveRole(config.RoleAnalyse)
	if err != nil {
		t.Fatal(err)
	}
	if r.Writable {
		t.Error("the analysis must never get a writable mount")
	}
	if r.Runner != h.eng.Claude {
		t.Error("the default analyse role should run through claude")
	}
	if r.Model == "" {
		t.Error("the analyse role should carry a model")
	}

	// The proposal's own model wins over the role's default.
	p, err := h.eng.StartProposal(context.Background(), h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if p.Model != h.eng.Cfg.AnalysisModel {
		t.Errorf("proposal model = %q, want the configured analysis model", p.Model)
	}
	_ = store.Proposal{}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
