package web

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"overseer/internal/config"
)

// ModelChoice is one option in a model dropdown.
type ModelChoice struct {
	// Provider is the group heading the option sits under.
	Provider string
	Model    string
	// Value is what the form submits: provider and model together, because a
	// model name alone is ambiguous once two providers serve the same one.
	Value string
	On    bool
}

// ProviderRow is one configured endpoint, as the settings pane shows it.
type ProviderRow struct {
	Name     string
	Kind     string
	Endpoint string
	KeyEnv   string
	// KeyPresent is whether the named variable is set in the daemon's
	// environment. The key itself is never read into the view.
	KeyPresent bool
	Models     []string
	// UsedBy names the roles pointing at this provider, so an operator can
	// see what a change would affect before making it.
	UsedBy []string
}

// RoleRow is one job in the loop and what currently runs it.
type RoleRow struct {
	Name        string
	Description string
	Agent       string
	Provider    string
	Model       string
	// Choices are the (provider, model) pairs this role may be switched to —
	// only those whose provider speaks the protocol its agent talks.
	Choices []ModelChoice
	// Agents are the CLIs this role may run through.
	Agents []Chip
	// Warning is set when the role is configured but cannot run.
	Warning string
}

// SettingsView is the providers-and-roles pane.
type SettingsView struct {
	Providers []ProviderRow
	Roles     []RoleRow
	Path      string
	// Editable is false when overseer has no config file path to write to.
	Editable bool
	Err      string
	Saved    bool
}

// modelChoices lists every (provider, model) pair an agent could use.
//
// The filter is the protocol: a role running through `claude` can only be
// pointed at an Anthropic-shaped endpoint, so offering it an OpenAI one would
// be offering a choice that fails validation on save.
func modelChoices(cfg config.Config, agent, curProvider, curModel string) []ModelChoice {
	want := ""
	switch agent {
	case config.AgentCodex:
		want = config.KindOpenAI
	default:
		want = config.KindAnthropic
	}

	var out []ModelChoice
	for _, name := range cfg.ProviderNames() {
		p := cfg.Providers[name]
		if p.Kind != want {
			continue
		}
		// A provider with no model list still offers its CLI's own default.
		models := p.Models
		if len(models) == 0 {
			models = []string{""}
		}
		for _, m := range models {
			label := m
			if label == "" {
				label = "(the CLI's default)"
			}
			out = append(out, ModelChoice{
				Provider: name,
				Model:    label,
				Value:    name + "/" + m,
				On:       name == curProvider && m == curModel,
			})
		}
	}
	return out
}

// buildSettings assembles the settings pane from the running configuration.
func (s *Server) buildSettings(q Query) SettingsView {
	// The live tables come from the engine, which owns them: the copy on the
	// server is the startup snapshot and would be stale after an edit.
	cfg := s.cfg
	cfg.Providers, cfg.Roles = s.eng.Roles()
	v := SettingsView{Path: s.cfgPath, Editable: s.cfgPath != ""}

	usedBy := map[string][]string{}
	for _, name := range config.RoleNames {
		if r, ok := cfg.Roles[name]; ok {
			usedBy[r.Provider] = append(usedBy[r.Provider], name)
		}
	}
	for _, name := range cfg.ProviderNames() {
		p := cfg.Providers[name]
		roles := usedBy[name]
		sort.Strings(roles)
		v.Providers = append(v.Providers, ProviderRow{
			Name:       name,
			Kind:       p.Kind,
			Endpoint:   p.Endpoint(),
			KeyEnv:     p.KeyEnv,
			KeyPresent: p.KeyPresent(),
			Models:     p.Models,
			UsedBy:     roles,
		})
	}

	for _, name := range config.RoleNames {
		r := cfg.Roles[name]
		row := RoleRow{
			Name:        name,
			Description: config.RoleDescriptions[name],
			Agent:       r.Agent,
			Provider:    r.Provider,
			Model:       r.Model,
			Choices:     modelChoices(cfg, r.Agent, r.Provider, r.Model),
		}
		if row.Model == "" {
			row.Model = "(the CLI's default)"
		}
		for _, a := range []string{config.AgentClaude, config.AgentCodex} {
			row.Agents = append(row.Agents, Chip{
				Label: a,
				On:    a == r.Agent,
				URL:   q.URL("overlay", "settings"),
			})
		}
		if p, ok := cfg.Providers[r.Provider]; ok && !p.KeyPresent() {
			row.Warning = fmt.Sprintf("%s is not set in the daemon's environment, so this role cannot authenticate.", p.KeyEnv)
		}
		v.Roles = append(v.Roles, row)
	}
	return v
}

// handleSettings applies a role change and writes it back to the config file.
//
// Only the roles are editable from here. Adding a provider means naming an
// endpoint and an environment variable, which is a config-file edit an
// operator makes deliberately — not something worth a form that could point
// the daemon at an arbitrary host on a stray click.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if s.cfgPath == "" {
		http.Error(w, "this daemon was started without a config file to write to",
			http.StatusBadRequest)
		return
	}

	providers, live := s.eng.Roles()
	roles := map[string]config.Role{}
	for name, cur := range live {
		roles[name] = cur
	}
	for _, name := range config.RoleNames {
		cur := roles[name]
		if a := strings.TrimSpace(r.FormValue("agent-" + name)); a != "" {
			cur.Agent = a
		}
		// The dropdown submits "provider/model" because a model name alone
		// is ambiguous once two providers serve the same one.
		if choice := strings.TrimSpace(r.FormValue("model-" + name)); choice != "" {
			provider, model, _ := strings.Cut(choice, "/")
			cur.Provider, cur.Model = provider, model
		}
		roles[name] = cur
	}

	if err := config.SaveProvidersAndRoles(s.cfgPath, providers, roles); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Re-read rather than trusting what was just sent: the file is the source
	// of truth, and reloading is the only way to be sure what the daemon now
	// runs is what is on disk.
	reloaded, err := config.Load(s.cfgPath)
	if err != nil {
		http.Error(w, "saved, but the file no longer loads: "+err.Error(),
			http.StatusInternalServerError)
		return
	}
	s.eng.SetRoles(reloaded.Providers, reloaded.Roles)

	q := ParseQuery(r)
	http.Redirect(w, r, q.URL("overlay", "settings", "saved", true), http.StatusSeeOther)
}

// analyseModels are the models the wizard may run one analysis on: whatever
// the analyse role's agent can reach, grouped by provider.
func (s *Server) analyseModels(current string) []ModelChoice {
	cfg := s.cfg
	cfg.Providers, cfg.Roles = s.eng.Roles()
	role := cfg.Roles[config.RoleAnalyse]

	provider := role.Provider
	model := role.Model
	if current != "" {
		// The proposal already picked one; keep it selected so a reload does
		// not silently move the analysis to a different model.
		model = current
	}
	return modelChoices(cfg, role.Agent, provider, model)
}

// analyseModel is the analyse role's model, for display before a proposal
// exists.
func (s *Server) analyseModel() string {
	_, roles := s.eng.Roles()
	if m := roles[config.RoleAnalyse].Model; m != "" {
		return m
	}
	return s.cfg.AnalysisModel
}
