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
	// Pasting the credential into key_env is the easy slip, and its symptom is
	// a role that cannot authenticate against a variable nobody ever set. The
	// shapes below are not environment variable names by POSIX, so they are
	// caught at load rather than mid-analysis.
	//
	// A token that happens to be spelled like a legal name — dcs_7abc — cannot
	// be told apart from one, and this deliberately does not guess. That case
	// is what `key:` is for, and the error says so.
	for _, bad := range []string{"sk-live-abc123 def", "sk-ant-api03-Zm9v", "7starts-with-a-digit"} {
		path := writeConfig(t, `
providers:
  x: {kind: openai, key_env: "`+bad+`", models: [m]}
roles:
  review: {agent: codex, provider: x, model: m}
`)
		err := func() error { _, err := Load(path); return err }()
		if err == nil {
			t.Errorf("%q: expected an error", bad)
			continue
		}
		if !strings.Contains(err.Error(), "`key:`") {
			t.Errorf("%q: the error should point at key:, got %v", bad, err)
		}
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
			RoleCode:      {Agent: AgentClaude, Provider: "anthropic", Model: "claude-opus-5"},
			RoleReview:    {Agent: AgentCodex, Provider: "openai"},
			RoleAnalyse:   {Agent: AgentClaude, Provider: "anthropic", Model: "claude-opus-5"},
			RoleArchitect: {Agent: AgentClaude, Provider: "anthropic", Model: "claude-opus-5"},
			RoleChat:      {Agent: AgentClaude, Provider: "anthropic", Model: "claude-opus-5"},
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

// The correction this split exists for: the default claude and codex providers
// run against the operator's subscription through the CLI's own login, so what
// those CLIs report is a usage signal, not a bill. Only an endpoint the
// operator supplied is money.
func TestOnlyABringYourOwnEndpointIsMetered(t *testing.T) {
	c := Default()
	for name, p := range c.Providers {
		if p.Metered() {
			t.Errorf("default provider %q reports as metered; it runs on the CLI's own login", name)
		}
	}

	c.Providers["inhouse"] = Provider{
		Kind:    KindOpenAI,
		BaseURL: "https://llm.internal.example/v1",
		KeyEnv:  "INHOUSE_KEY",
	}
	if !c.Providers["inhouse"].Metered() {
		t.Error("a provider with its own base_url is the operator's own money and must be metered")
	}

	metered := c.MeteredProviders()
	if !metered["inhouse"] {
		t.Errorf("MeteredProviders = %v, want inhouse", metered)
	}
	if len(metered) != 1 {
		t.Errorf("MeteredProviders = %v, want only the configured endpoint", metered)
	}
}

// An operator's existing config file names three roles. Adding a fourth must
// not make it unloadable.
func TestAConfigWithoutTheArchitectRoleStillLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
roles:
  code:    {agent: claude, provider: anthropic}
  review:  {agent: codex,  provider: openai}
  analyse: {agent: claude, provider: anthropic, model: claude-sonnet-5}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("a config predating the architect role failed to load: %v", err)
	}
	if _, ok := c.Roles[RoleArchitect]; !ok {
		t.Error("the architect role did not fall back to its default")
	}
	if c.Roles[RoleCode].Provider != "anthropic" {
		t.Errorf("the file's own roles were lost: %+v", c.Roles)
	}
}

func TestTheChatIsItsOwnRoleOnACheaperModel(t *testing.T) {
	// The chat is used casually and often. Sharing the architect's role would
	// put every "what does this do?" on the strongest model, and the only way
	// to lower it would also lower the one conversation that wants opus.
	c := Default()
	role, ok := c.Roles[RoleChat]
	if !ok {
		t.Fatal("the chat has no default role")
	}
	if role.Model != "claude-sonnet-5" {
		t.Errorf("chat model = %q, want claude-sonnet-5", role.Model)
	}
	if role.Agent != AgentClaude {
		t.Errorf("chat agent = %q, want %q", role.Agent, AgentClaude)
	}
	if !contains(RoleNames, RoleChat) {
		t.Error("the chat is missing from RoleNames, so the settings pane will not show it")
	}
	if RoleDescriptions[RoleChat] == "" {
		t.Error("the chat has no description for the settings pane")
	}
	if err := c.validateProviders(); err != nil {
		t.Errorf("the defaults must validate: %v", err)
	}
}

func TestAConfigFileNamingOnlyTheOlderRolesStillLoads(t *testing.T) {
	// Every existing config file predates this role. Load unmarshals over
	// Default(), so the map is merged rather than replaced — but that is the
	// property the upgrade depends on, so it is worth pinning.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(path, []byte(`
roles:
  code:
    agent: claude
    provider: anthropic
  review:
    agent: codex
    provider: openai
`), 0o644)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Roles[RoleChat].Agent == "" {
		t.Error("a config file that predates the chat role should still get its default")
	}
}

func TestAKeyCanBeSetInTheConfigFile(t *testing.T) {
	// An in-house endpoint needs a credential, and requiring a second file to
	// hold it buys nothing: both are files on disk, owned by the same user,
	// caught by the same backup. The one thing that does differ is who can
	// read it, which is what the mode check below is for.
	path := writeConfig(t, `
providers:
  inhouse:
    kind: openai
    base_url: https://llm.internal.example/v1
    key: dcs_7XXXplaceholder
    models: [kimi-k3]
roles:
  analyse: {agent: codex, provider: inhouse, model: kimi-k3}
`)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := c.Providers["inhouse"]
	if got := p.Credential(); got != "dcs_7XXXplaceholder" {
		t.Errorf("Credential() = %q, want the key from the file", got)
	}
	if !p.KeyPresent() {
		t.Error("a provider carrying its own key has a key present")
	}
}

func TestKeyEnvStillWorksAndTheTwoAreNotBothAllowed(t *testing.T) {
	// key_env stays the right answer for anyone who already has the secret in
	// their environment. Naming both is ambiguous — a reader cannot tell which
	// one is live — so it is refused rather than silently ranked.
	t.Setenv("INHOUSE_LLM_KEY", "from-the-environment")
	path := writeConfig(t, `
providers:
  inhouse:
    kind: openai
    base_url: https://llm.internal.example/v1
    key_env: INHOUSE_LLM_KEY
    models: [kimi-k3]
roles:
  analyse: {agent: codex, provider: inhouse, model: kimi-k3}
`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Providers["inhouse"].Credential(); got != "from-the-environment" {
		t.Errorf("Credential() = %q, want the environment's value", got)
	}

	both := writeConfig(t, `
providers:
  inhouse:
    kind: openai
    base_url: https://llm.internal.example/v1
    key: in-the-file
    key_env: INHOUSE_LLM_KEY
    models: [kimi-k3]
`)
	if err := os.Chmod(both, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(both); err == nil {
		t.Error("naming both key and key_env should be refused as ambiguous")
	}
}

func TestAKeyInAReadableConfigFileIsRefused(t *testing.T) {
	// The one real difference between holding the secret here and holding it in
	// the environment: this file's mode is visible and checkable, so it gets
	// checked. Same posture ssh takes over a private key, and the same fix.
	path := writeConfig(t, `
providers:
  inhouse:
    kind: openai
    base_url: https://llm.internal.example/v1
    key: dcs_7XXXplaceholder
    models: [kimi-k3]
`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("a key in a world-readable config should be refused")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("the error should say how to fix it, got: %v", err)
	}
}

func TestTheModeIsOnlyCheckedWhenAKeyIsActuallyInTheFile(t *testing.T) {
	// Every existing config file is 0644 and contains no secret. Refusing
	// those would break every install to protect nothing.
	path := writeConfig(t, `
providers:
  inhouse:
    kind: openai
    base_url: https://llm.internal.example/v1
    key_env: INHOUSE_LLM_KEY
    models: [kimi-k3]
`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Errorf("a config with no key in it should load at any mode: %v", err)
	}
}
