package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"overseer/internal/config"
)

func TestSettingsPaneListsRolesAndProviders(t *testing.T) {
	s, _ := newTestServer(t)
	body := get(t, s, "/?overlay=settings").Body.String()
	for _, want := range []string{
		"Models and providers", "code", "review", "analyse",
		"anthropic", "openai", "/settings",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings pane missing %q", want)
		}
	}
}

func TestModelDropdownOnlyOffersProvidersTheAgentCanSpeakTo(t *testing.T) {
	// Offering a claude role an OpenAI endpoint would be offering a choice
	// that fails validation the moment it is saved.
	cfg := config.Default()
	cfg.Providers["inhouse"] = config.Provider{
		Kind: config.KindOpenAI, BaseURL: "https://llm.dc.internal/v1",
		KeyEnv: "DC_LLM_KEY", Models: []string{"qwen3-coder-480b"},
	}

	claude := modelChoices(cfg, config.AgentClaude, "anthropic", "claude-opus-5")
	for _, c := range claude {
		if c.Provider == "inhouse" {
			t.Errorf("a claude role was offered %q, an OpenAI endpoint", c.Provider)
		}
	}
	var sawSelected bool
	for _, c := range claude {
		if c.On && c.Model == "claude-opus-5" {
			sawSelected = true
		}
	}
	if !sawSelected {
		t.Error("the current model should be the selected option")
	}

	codex := modelChoices(cfg, config.AgentCodex, "inhouse", "qwen3-coder-480b")
	var sawInhouse bool
	for _, c := range codex {
		if c.Provider == "anthropic" {
			t.Errorf("a codex role was offered %q, an Anthropic endpoint", c.Provider)
		}
		if c.Provider == "inhouse" {
			sawInhouse = true
		}
	}
	if !sawInhouse {
		t.Error("the codex role should be offered the OpenAI-compatible provider")
	}
}

func TestModelDropdownValueCarriesTheProvider(t *testing.T) {
	// Two providers can serve the same model name, so a bare model name is
	// ambiguous and would silently pick whichever the map iterated first.
	cfg := config.Default()
	cfg.Providers["mirror"] = config.Provider{
		Kind: config.KindAnthropic, BaseURL: "https://mirror.internal/v1",
		KeyEnv: "MIRROR_KEY", Models: []string{"claude-opus-5"},
	}
	for _, c := range modelChoices(cfg, config.AgentClaude, "anthropic", "claude-opus-5") {
		if !strings.Contains(c.Value, "/") {
			t.Errorf("choice %+v has no provider in its value", c)
		}
	}
}

func TestSettingsSaveRewritesTheRoleAndReloadsIt(t *testing.T) {
	s, _ := newTestServer(t)
	if err := os.WriteFile(s.cfgPath, []byte("# keep me\nmax_parallel: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := post(t, s, "/settings", url.Values{
		"agent-code":    {"claude"},
		"model-code":    {"anthropic/claude-haiku-4-5"},
		"agent-review":  {"codex"},
		"model-review":  {"openai/"},
		"agent-analyse": {"claude"},
		"model-analyse": {"anthropic/claude-sonnet-5"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	// The daemon is now running the new role, without a restart.
	_, roles := s.eng.Roles()
	if roles[config.RoleCode].Model != "claude-haiku-4-5" {
		t.Errorf("code role = %+v, want the saved model live in the engine", roles[config.RoleCode])
	}

	// And the file keeps everything that was not edited.
	body, err := os.ReadFile(s.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "# keep me") {
		t.Error("the save lost a comment from the config file")
	}
	if !strings.Contains(string(body), "claude-haiku-4-5") {
		t.Error("the save did not write the new model")
	}
}

func TestSettingsSaveRefusesAProtocolMismatch(t *testing.T) {
	s, _ := newTestServer(t)
	if err := os.WriteFile(s.cfgPath, []byte("max_parallel: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := post(t, s, "/settings", url.Values{
		"agent-code":    {"claude"},
		"model-code":    {"openai/"}, // claude cannot talk to an OpenAI endpoint
		"agent-review":  {"codex"},
		"model-review":  {"openai/"},
		"agent-analyse": {"claude"},
		"model-analyse": {"anthropic/claude-sonnet-5"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "speaks") {
		t.Errorf("body = %q, want the protocol mismatch explained", rec.Body.String())
	}
	// The engine must be unchanged.
	_, roles := s.eng.Roles()
	if roles[config.RoleCode].Provider == "openai" {
		t.Error("a refused edit reached the engine anyway")
	}
}

func TestSettingsRejectsACrossSiteRequest(t *testing.T) {
	// It writes to a file on disk and repoints where money is spent.
	s, _ := newTestServer(t)
	req, err := http.NewRequest(http.MethodPost, "/settings", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = s.cfg.ListenAddr
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestSettingsWarnsWhenAProviderHasNoKey(t *testing.T) {
	// A missing key surfaces as an authentication failure that pauses the
	// whole run; saying so up front is cheaper than discovering it that way.
	s, _ := newTestServer(t)
	providers, roles := s.eng.Roles()
	providers["inhouse"] = config.Provider{
		Kind: config.KindOpenAI, BaseURL: "https://llm.dc.internal/v1",
		KeyEnv: "OVERSEER_TEST_NO_SUCH_KEY", Models: []string{"m"},
	}
	roles[config.RoleReview] = config.Role{
		Agent: config.AgentCodex, Provider: "inhouse", Model: "m",
	}
	s.eng.SetRoles(providers, roles)

	body := get(t, s, "/?overlay=settings").Body.String()
	if !strings.Contains(body, "OVERSEER_TEST_NO_SUCH_KEY") {
		t.Error("the pane should name the variable that is not set")
	}
	if !strings.Contains(body, "not set in the daemon's environment") {
		t.Error("the pane should say the key is missing")
	}
}

func TestWizardModelIsADropdownOfConfiguredModels(t *testing.T) {
	s, _ := newTestServer(t)
	choices := s.analyseModels("")
	if len(choices) == 0 {
		t.Fatal("the wizard should offer the analyse role's models")
	}
	for _, c := range choices {
		if c.Provider != "anthropic" {
			t.Errorf("choice %+v: the default analyse role runs through claude", c)
		}
	}
	// The proposal's own model stays selected across a reload.
	picked := s.analyseModels("claude-haiku-4-5")
	var on string
	for _, c := range picked {
		if c.On {
			on = c.Model
		}
	}
	if on != "claude-haiku-4-5" {
		t.Errorf("selected = %q, want the proposal's own model", on)
	}
}
