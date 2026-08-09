package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/agent"
	"overseer/internal/config"
	"overseer/internal/sandbox"
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
	// The endpoint reaches codex on its command line, not in its environment:
	// with a ChatGPT login it ignores OPENAI_BASE_URL entirely.
	if r.BaseURL != "https://llm.dc.internal/v1" {
		t.Errorf("BaseURL = %q, want the endpoint", r.BaseURL)
	}
	if r.Env[agent.CodexKeyEnv] != "secret-value" {
		t.Errorf("env did not carry the key read from OVERSEER_TEST_LLM_KEY")
	}
	joined := strings.Join(r.args("go", "", "", "", ""), " ")
	if !strings.Contains(joined, `base_url="https://llm.dc.internal/v1"`) {
		t.Errorf("argv does not route to the endpoint: %s", joined)
	}

	// And the argv carries the model.
	args := r.args("prompt", "", "", "", "")
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

func TestAClaudeReviewerIsHeldToTheSchemaToo(t *testing.T) {
	// This used to assert the opposite, because `claude -p` had no way to be
	// held to a schema and the strict parser was the only guarantee left.
	// Claude Code gained --json-schema, so a claude reviewer now gets the same
	// enforcement a codex one does — and still no codex-only flags.
	h := newHarness(t, "true", "true")
	h.eng.Cfg.Roles[config.RoleReview] = config.Role{
		Agent: config.AgentClaude, Provider: "anthropic",
	}
	r, err := h.eng.resolveRole(config.RoleReview)
	if err != nil {
		t.Fatal(err)
	}
	if !r.structured() {
		t.Fatal("a claude reviewer should be held to the verdict schema")
	}

	args := r.args("Review the diff.", "", "", "", string(agent.VerdictSchema))
	if !containsArg(args, "--json-schema") {
		t.Errorf("args = %v, want the schema enforced", args)
	}
	for _, unwanted := range []string{"--output-schema", "--output-last-message"} {
		if containsArg(args, unwanted) {
			t.Errorf("args = %v, want no %s for a claude reviewer", args, unwanted)
		}
	}
}

func TestTheInlineContractIsStillThereForAnEndpointThatCannotEnforce(t *testing.T) {
	// structured_output: false falls back to stating the contract in the
	// prompt. Dropping it silently would leave the reviewer with no stated
	// contract at all.
	prompt := withInlineSchema("Review the diff.")
	for _, want := range []string{"changes_requested", "findings", "severity"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the inline schema is missing %q", want)
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

// Both CLIs sandbox their own shell tool, and a sandbox inside a sandbox is
// refused on kernels that gate unprivileged user namespaces behind an AppArmor
// profile. Each agent is told to skip its own — claude by an environment
// variable, codex by a flag — which is what stops "bwrap: No permissions to
// create a new namespace" appearing in every transcript as if overseer's own
// sandbox were broken.
//
// The sandbox is set before the role is resolved, because Confined is a
// property of the resolved role rather than something read at dispatch time.
func TestConfinedAgentsSkipTheirOwnSandbox(t *testing.T) {
	h := newHarness(t, "true", "true")
	h.eng.Sandbox = fakeWrapper{name: "bwrap"}

	code, err := h.eng.resolveRole(config.RoleCode)
	if err != nil {
		t.Fatal(err)
	}
	if !code.Confined {
		t.Fatal("a role resolved under a sandbox is not marked confined")
	}
	if got := h.eng.agentEnv(code)["CLAUDE_CODE_SANDBOXED"]; got != "1" {
		t.Errorf("CLAUDE_CODE_SANDBOXED = %q, want 1", got)
	}

	review, err := h.eng.resolveRole(config.RoleReview)
	if err != nil {
		t.Fatal(err)
	}
	args := review.args("p", "", "", "", "")
	if !hasArg(args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("codex argv = %v, want it to skip building its own sandbox", args)
	}
	// codex implements every sandbox mode with bubblewrap, danger-full-access
	// included, so -s of any value would still nest.
	if hasArg(args, "-s") {
		t.Errorf("codex argv = %v, want no -s at all when externally sandboxed", args)
	}
}

// With the sandbox off, each agent's own sandbox is the only one there is.
// Claiming otherwise turns a deliberate "no sandbox" into an unprotected agent
// that believes it is protected.
func TestUnconfinedAgentsKeepTheirOwnSandbox(t *testing.T) {
	for _, wrapper := range []sandbox.Wrapper{sandbox.Passthrough{}, nil} {
		h := newHarness(t, "true", "true")
		h.eng.Sandbox = wrapper

		code, err := h.eng.resolveRole(config.RoleCode)
		if err != nil {
			t.Fatal(err)
		}
		if code.Confined {
			t.Errorf("wrapper %v: role marked confined with no sandbox", wrapper)
		}
		if _, ok := h.eng.agentEnv(code)["CLAUDE_CODE_SANDBOXED"]; ok {
			t.Errorf("wrapper %v: an unsandboxed agent was told it is confined", wrapper)
		}

		review, err := h.eng.resolveRole(config.RoleReview)
		if err != nil {
			t.Fatal(err)
		}
		args := review.args("p", "", "", "", "")
		if !hasArg(args, "-s") || !hasArg(args, "read-only") {
			t.Errorf("wrapper %v: codex argv = %v, want -s read-only kept", wrapper, args)
		}
		if hasArg(args, "--dangerously-bypass-approvals-and-sandbox") {
			t.Errorf("wrapper %v: codex told to skip its sandbox with none of ours in place", wrapper)
		}
	}
}

// agentEnv must not write through to the role's own map, which callers hold.
func TestAgentEnvDoesNotMutateTheRolesMap(t *testing.T) {
	h := newHarness(t, "true", "true")
	h.eng.Sandbox = fakeWrapper{name: "bwrap"}
	role, err := h.eng.resolveRole(config.RoleCode)
	if err != nil {
		t.Fatal(err)
	}
	h.eng.agentEnv(role)
	if _, ok := role.Env["CLAUDE_CODE_SANDBOXED"]; ok {
		t.Error("agentEnv wrote into the role's environment map")
	}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// fakeWrapper stands in for a confining sandbox without needing bwrap to be
// usable on the machine running the tests.
type fakeWrapper struct{ name string }

func (f fakeWrapper) Wrap(bin string, args []string, _ sandbox.Spec) (string, []string) {
	return bin, args
}
func (f fakeWrapper) Name() string { return f.name }

func TestClaudeEnforcesASchemaWhenGivenOne(t *testing.T) {
	// The comment this replaces said claude had no way to be held to a schema,
	// so a claude-backed reviewer or analysis got the contract as prose in its
	// prompt and the parser was the only thing standing behind it. Claude Code
	// gained --json-schema; enforcing it means a reply of the wrong shape
	// cannot be produced at all.
	h := newHarness(t, "true", "true")
	r, err := h.eng.resolveRole(config.RoleCode)
	if err != nil {
		t.Fatal(err)
	}
	if !r.structured() {
		t.Fatal("a claude role should now be able to enforce a schema")
	}

	schema := `{"type":"object"}`
	args := r.args("go", "", "", "", schema)
	found := ""
	for i, a := range args {
		if a == "--json-schema" && i+1 < len(args) {
			found = args[i+1]
		}
	}
	if found != schema {
		t.Errorf("--json-schema = %q, want the schema", found)
	}
}

func TestAProviderCanTurnSchemaEnforcementOff(t *testing.T) {
	// For a gateway that mishandles the constrained tool call. The schema then
	// falls back to being stated in the prompt, which is what it was before.
	h := newHarness(t, "true", "true")
	off := false
	h.eng.SetRoles(
		map[string]config.Provider{
			"anthropic": {Kind: config.KindAnthropic},
			"openai":    {Kind: config.KindOpenAI},
			"shaky":     {Kind: config.KindAnthropic, BaseURL: "https://gw.example/", Key: "k", EnforceSchema: &off},
		},
		map[string]config.Role{
			"code":      {Agent: config.AgentClaude, Provider: "shaky"},
			"review":    {Agent: config.AgentCodex, Provider: "openai"},
			"analyse":   {Agent: config.AgentClaude, Provider: "anthropic"},
			"architect": {Agent: config.AgentClaude, Provider: "anthropic"},
			"chat":      {Agent: config.AgentClaude, Provider: "anthropic"},
		})
	r, err := h.eng.resolveRole(config.RoleCode)
	if err != nil {
		t.Fatal(err)
	}
	if r.structured() {
		t.Error("the provider turned enforcement off and it stayed on")
	}
	for _, a := range r.args("go", "", "", "", `{"type":"object"}`) {
		if a == "--json-schema" {
			t.Fatal("enforcement is off, so the flag must not appear")
		}
	}
}

func TestAProviderCanRaiseTheOutputCeiling(t *testing.T) {
	// Claude Code sends max_tokens: 32000 by default, and an endpoint whose
	// thinking is unbounded can spend the whole allowance before it starts
	// answering — the reply then truncates mid-stream. This is the way to give
	// it headroom without touching the roles that do not need it.
	h := newHarness(t, "true", "true")
	h.eng.SetRoles(
		map[string]config.Provider{
			"anthropic": {Kind: config.KindAnthropic},
			"openai":    {Kind: config.KindOpenAI},
			"gw": {Kind: config.KindAnthropic, BaseURL: "https://gw.example/", Key: "k",
				MaxOutputTokens: 100000},
		},
		map[string]config.Role{
			"code":      {Agent: config.AgentClaude, Provider: "gw"},
			"review":    {Agent: config.AgentCodex, Provider: "openai"},
			"analyse":   {Agent: config.AgentClaude, Provider: "anthropic"},
			"architect": {Agent: config.AgentClaude, Provider: "anthropic"},
			"chat":      {Agent: config.AgentClaude, Provider: "anthropic"},
		})

	raised, err := h.eng.resolveRole(config.RoleCode)
	if err != nil {
		t.Fatal(err)
	}
	if got := raised.Env[agent.ClaudeMaxOutputEnv]; got != "100000" {
		t.Errorf("%s = %q, want 100000", agent.ClaudeMaxOutputEnv, got)
	}

	// A provider that says nothing leaves the CLI's own default alone.
	plain, err := h.eng.resolveRole(config.RoleAnalyse)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plain.Env[agent.ClaudeMaxOutputEnv]; ok {
		t.Errorf("env = %v, want no ceiling imposed", plain.Env)
	}
}
