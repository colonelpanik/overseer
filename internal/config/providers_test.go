package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultsReproduceTheOriginalLoop(t *testing.T) {
	// A config file that says nothing about providers must behave exactly as
	// it did before providers existed: Claude writes, Codex reviews.
	c := Default()
	for _, tc := range []struct{ role, agent, provider string }{
		{RoleCode, AgentClaude, "anthropic"},
		{RoleReview, AgentCodex, "openai"},
		{RoleAnalyse, AgentClaude, "anthropic"},
	} {
		r, p, err := c.Resolve(tc.role)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", tc.role, err)
		}
		if r.Agent != tc.agent || r.Provider != tc.provider {
			t.Errorf("role %q = %+v, want agent %s provider %s", tc.role, r, tc.agent, tc.provider)
		}
		if p.Kind == "" {
			t.Errorf("provider for %q has no kind", tc.role)
		}
	}
	if err := c.Validate(); err != nil {
		t.Errorf("the defaults must validate: %v", err)
	}
}

func TestOneInHouseProviderKeepsTheVendorOnes(t *testing.T) {
	// Adding a provider is additive: an operator naming their own endpoint
	// should not lose the two that were already there.
	path := writeConfig(t, `
providers:
  inhouse:
    kind: openai
    base_url: https://llm.dc.internal/v1
    key_env: DC_LLM_KEY
    models: [qwen3-coder-480b]
roles:
  review: {agent: codex, provider: inhouse, model: qwen3-coder-480b}
`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Providers) != 3 {
		t.Errorf("providers = %v, want the two defaults plus inhouse", c.ProviderNames())
	}
	r, p, err := c.Resolve(RoleReview)
	if err != nil {
		t.Fatal(err)
	}
	if r.Model != "qwen3-coder-480b" || p.BaseURL != "https://llm.dc.internal/v1" {
		t.Errorf("review = %+v / %+v", r, p)
	}
	// The untouched roles keep their defaults.
	code, _, err := c.Resolve(RoleCode)
	if err != nil {
		t.Fatal(err)
	}
	if code.Agent != AgentClaude {
		t.Errorf("code role = %+v, want the default", code)
	}
}

func TestRoleMayPickEitherAgent(t *testing.T) {
	// Free assignment is the point: review through claude, code through an
	// OpenAI-compatible endpoint.
	path := writeConfig(t, `
providers:
  inhouse: {kind: openai, base_url: https://llm.dc.internal/v1, key_env: DC_LLM_KEY, models: [qwen3-coder-480b]}
roles:
  code:   {agent: codex,  provider: inhouse, model: qwen3-coder-480b}
  review: {agent: claude, provider: anthropic, model: claude-opus-5}
`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r, _, _ := c.Resolve(RoleCode); r.Agent != AgentCodex {
		t.Errorf("code agent = %q, want codex", r.Agent)
	}
	if r, _, _ := c.Resolve(RoleReview); r.Agent != AgentClaude {
		t.Errorf("review agent = %q, want claude", r.Agent)
	}
}

func TestProtocolMismatchIsRejected(t *testing.T) {
	// The one constraint free assignment cannot escape: each CLI speaks one
	// protocol, so the provider has to match. Caught at load, not halfway
	// through a paid-for task.
	path := writeConfig(t, `
roles:
  code: {agent: claude, provider: openai}
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "speaks") {
		t.Errorf("error = %v, want it to explain the protocol mismatch", err)
	}
}

func TestConfigRejectsBadProviders(t *testing.T) {
	cases := map[string]string{
		"unknown kind":     "providers:\n  x: {kind: bedrock, key_env: K}\nroles:\n  code: {agent: claude, provider: x}\n",
		"unknown agent":    "roles:\n  code: {agent: gemini, provider: anthropic}\n",
		"missing provider": "roles:\n  code: {agent: claude, provider: nowhere}\n",
		"model not listed": "roles:\n  analyse: {agent: claude, provider: anthropic, model: claude-9}\n",
		"non-http base":    "providers:\n  x: {kind: openai, base_url: 'ext::sh -c id', key_env: K}\nroles:\n  review: {agent: codex, provider: x}\n",
	}
	for name, body := range cases {
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestKeyEnvMustBeAVariableNameNotAKey(t *testing.T) {
	// A key pasted into key_env would end up in backups and version control.
	// The check is crude on purpose: anything that looks like a value rather
	// than a name is refused.
	path := writeConfig(t, `
providers:
  x: {kind: openai, key_env: "sk-live-abc123 def", models: [m]}
roles:
  review: {agent: codex, provider: x, model: m}
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "NAME of an environment variable") {
		t.Errorf("error = %v", err)
	}
}

func TestLegacyAnalysisModelStillApplies(t *testing.T) {
	// A config written before roles existed must keep working.
	path := writeConfig(t, "analysis_model: claude-haiku-4-5\n")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r, _, _ := c.Resolve(RoleAnalyse); r.Model != "claude-haiku-4-5" {
		t.Errorf("analyse model = %q, want the legacy analysis_model", r.Model)
	}

	// An explicit role wins over the legacy key.
	path = writeConfig(t, "analysis_model: claude-haiku-4-5\nroles:\n  analyse: {agent: claude, provider: anthropic, model: claude-opus-5}\n")
	c, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r, _, _ := c.Resolve(RoleAnalyse); r.Model != "claude-opus-5" {
		t.Errorf("analyse model = %q, want the explicit role to win", r.Model)
	}
}

func TestKeyPresentReadsTheNamedVariable(t *testing.T) {
	p := Provider{KeyEnv: "OVERSEER_TEST_KEY_ABSENT"}
	if p.KeyPresent() {
		t.Error("an unset variable should report absent")
	}
	t.Setenv("OVERSEER_TEST_KEY_ABSENT", "x")
	if !p.KeyPresent() {
		t.Error("a set variable should report present")
	}
	// A provider naming no variable relies on the CLI's own stored login.
	if !(Provider{}).KeyPresent() {
		t.Error("a provider with no key_env should not report a missing key")
	}
}

func TestSaveProvidersAndRolesKeepsTheRestOfTheFile(t *testing.T) {
	// An operator who changed one dropdown has not consented to losing the
	// comments and ordering in their config file.
	path := writeConfig(t, `# my overseer config
listen_addr: 127.0.0.1:9999   # not the default
max_parallel: 6

# how strict the reviewer is
blocking_severity: major
`)
	err := SaveProvidersAndRoles(path,
		map[string]Provider{
			"anthropic": {Kind: KindAnthropic, KeyEnv: "ANTHROPIC_API_KEY", Models: []string{"claude-opus-5"}},
			"openai":    {Kind: KindOpenAI, KeyEnv: "OPENAI_API_KEY"},
		},
		map[string]Role{
			RoleCode:    {Agent: AgentClaude, Provider: "anthropic", Model: "claude-opus-5"},
			RoleReview:  {Agent: AgentCodex, Provider: "openai"},
			RoleAnalyse: {Agent: AgentClaude, Provider: "anthropic", Model: "claude-opus-5"},
		})
	if err != nil {
		t.Fatalf("SaveProvidersAndRoles: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{
		"# my overseer config",
		"# not the default",
		"# how strict the reviewer is",
		"listen_addr: 127.0.0.1:9999",
		"providers:",
		"roles:",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("saved config lost %q:\n%s", want, text)
		}
	}

	// And it still loads, with the edit applied.
	c, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if c.ListenAddr != "127.0.0.1:9999" || c.MaxParallel != 6 {
		t.Errorf("unrelated settings changed: %+v", c)
	}
	if r, _, _ := c.Resolve(RoleCode); r.Model != "claude-opus-5" {
		t.Errorf("code model = %q, want the saved value", r.Model)
	}
}

func TestSaveProvidersAndRolesRefusesAnInvalidEdit(t *testing.T) {
	// Writing a config the daemon would refuse to load is worse than
	// refusing the edit.
	path := writeConfig(t, "max_parallel: 2\n")
	err := SaveProvidersAndRoles(path,
		map[string]Provider{"openai": {Kind: KindOpenAI, KeyEnv: "OPENAI_API_KEY"}},
		map[string]Role{
			RoleCode:    {Agent: AgentClaude, Provider: "openai"}, // protocol mismatch
			RoleReview:  {Agent: AgentCodex, Provider: "openai"},
			RoleAnalyse: {Agent: AgentCodex, Provider: "openai"},
		})
	if err == nil {
		t.Fatal("expected an error")
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "providers:") {
		t.Error("a refused edit must not have been written")
	}
}

func TestSaveProvidersAndRolesCreatesAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	err := SaveProvidersAndRoles(path, defaultProviders(), defaultRoles())
	if err != nil {
		t.Fatalf("SaveProvidersAndRoles: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Errorf("the written file should load: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The file names the variables holding credentials.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %v, want 0600", perm)
	}
}
