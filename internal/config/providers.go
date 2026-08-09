package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Agent kinds — the CLI a role is driven through.
const (
	AgentClaude = "claude"
	AgentCodex  = "codex"
)

// Provider kinds — the wire protocol an endpoint speaks.
const (
	KindAnthropic = "anthropic"
	KindOpenAI    = "openai"
)

// Roles. Each names one job in the loop and binds it to an agent, a provider
// and a model.
const (
	RoleCode    = "code"
	RoleReview  = "review"
	RoleAnalyse = "analyse"
	// RoleArchitect is the design conversation: the operator and an agent
	// working out what to build before anything is queued. It is separate from
	// analyse because it wants a different model — analyse defaults to a cheap
	// one for reading, and this decides everything downstream.
	RoleArchitect = "architect"
	// RoleChat answers questions about a repository. Separate from architect
	// for the same reason architect is separate from analyse: this one is used
	// casually and often, and sharing a role would mean the only way to make
	// "what does this do?" cheaper is to make the design conversation cheaper
	// too.
	RoleChat = "chat"
)

// RoleNames is every role, in the order the dashboard shows them.
var RoleNames = []string{RoleCode, RoleReview, RoleAnalyse, RoleArchitect, RoleChat}

// RoleDescriptions explains what each role does, for the settings pane.
var RoleDescriptions = map[string]string{
	RoleCode:      "writes the plan and the implementation",
	RoleReview:    "reviews the plan and the diff, and produces the verdict",
	RoleAnalyse:   "reads a repository and proposes a task list",
	RoleArchitect: "talks a design through with you, then proposes the tasks",
	RoleChat:      "answers questions about a repository, and turns the answers into work",
}

// Provider is an endpoint overseer can point an agent CLI at.
//
// A provider is not an API client: overseer drives the `claude` and `codex`
// CLIs, and those CLIs do the file editing and command running that the loop
// depends on. A provider therefore configures *how a CLI is invoked* — which
// endpoint it talks to, which environment variable holds the key, and which
// models it may be asked for.
type Provider struct {
	// Kind is the wire protocol: anthropic or openai. It is what decides
	// which CLI can use this provider, because a CLI speaks one of them.
	Kind string `yaml:"kind"`
	// BaseURL is the endpoint. Empty means the CLI's own default, which is
	// how the vendor-hosted providers are configured.
	BaseURL string `yaml:"base_url"`
	// KeyEnv names the environment variable holding the API key. The key
	// itself is never stored in the config file or the database, and never
	// logged: only the operator's environment holds it.
	KeyEnv string `yaml:"key_env"`
	// Models are the models this provider may be asked for. The dashboard's
	// dropdown is exactly this list.
	Models []string `yaml:"models"`
}

// Role binds one job in the loop to an agent, a provider and a model.
type Role struct {
	// Agent is the CLI that runs this role: claude or codex.
	Agent string `yaml:"agent"`
	// Provider names an entry in Providers.
	Provider string `yaml:"provider"`
	// Model is empty for the CLI's own default.
	Model string `yaml:"model"`
}

// agentProtocol is the wire protocol each CLI speaks. This is the constraint
// that role assignment cannot escape: `claude` talks to an Anthropic-shaped
// endpoint and `codex` to an OpenAI-shaped one, so a role may pick either CLI
// but its provider has to match whichever it picked.
var agentProtocol = map[string]string{
	AgentClaude: KindAnthropic,
	AgentCodex:  KindOpenAI,
}

// defaultProviders reproduce the behaviour of a config file that says nothing
// about providers: each CLI talking to its own vendor with its own key.
func defaultProviders() map[string]Provider {
	return map[string]Provider{
		"anthropic": {
			Kind:   KindAnthropic,
			KeyEnv: "ANTHROPIC_API_KEY",
			Models: []string{
				"claude-opus-5",
				"claude-sonnet-5",
				"claude-haiku-4-5",
			},
		},
		"openai": {
			Kind:   KindOpenAI,
			KeyEnv: "OPENAI_API_KEY",
		},
	}
}

// defaultRoles keep the loop exactly as it was before roles existed: Claude
// writes, Codex reviews, and the analysis runs on the cheaper Claude model.
func defaultRoles() map[string]Role {
	return map[string]Role{
		RoleCode:    {Agent: AgentClaude, Provider: "anthropic"},
		RoleReview:  {Agent: AgentCodex, Provider: "openai"},
		RoleAnalyse: {Agent: AgentClaude, Provider: "anthropic", Model: "claude-sonnet-5"},
		// The strongest model by default: this conversation decides what
		// everything else builds.
		RoleArchitect: {Agent: AgentClaude, Provider: "anthropic", Model: "claude-opus-5"},
		// A middling model rather than the strongest: this one answers a
		// question at a time, many times a day, and it reads rather than
		// decides. Note it is absent from roleWrites, like the architect — a
		// conversation about a repository must never be able to edit it.
		RoleChat: {Agent: AgentClaude, Provider: "anthropic", Model: "claude-sonnet-5"},
	}
}

// ProviderNames returns the configured provider names, sorted, so every
// listing of them is in the same order.
func (c Config) ProviderNames() []string {
	names := make([]string, 0, len(c.Providers))
	for name := range c.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Resolve looks up a role and the provider it points at.
func (c Config) Resolve(role string) (Role, Provider, error) {
	r, ok := c.Roles[role]
	if !ok {
		return Role{}, Provider{}, fmt.Errorf("no role %q is configured", role)
	}
	p, ok := c.Providers[r.Provider]
	if !ok {
		return Role{}, Provider{}, fmt.Errorf("role %q names provider %q, which is not configured", role, r.Provider)
	}
	return r, p, nil
}

// Bin returns the binary that runs a role's agent.
func (c Config) Bin(agent string) string {
	if agent == AgentCodex {
		return c.CodexBin
	}
	return c.ClaudeBin
}

// KeyPresent reports whether a provider's key variable is set in the daemon's
// environment. The dashboard shows this so a missing key is visible before a
// task spends an iteration discovering it.
func (p Provider) KeyPresent() bool {
	return p.KeyEnv == "" || os.Getenv(p.KeyEnv) != ""
}

// Metered reports whether usage against this provider is real money to the
// operator.
//
// The default claude and codex providers run against the operator's own
// subscription through the CLI's stored login: what those CLIs report as
// total_cost_usd is what the usage *would* have cost through the API — a usage
// signal, not a bill. A provider is only metered once the operator has pointed
// it at an endpoint of their own, which is what base_url means: bring your own
// model, on your own account.
func (p Provider) Metered() bool { return p.BaseURL != "" }

// MeteredProviders names the providers whose usage is billed to the operator,
// for the accounting split.
func (c Config) MeteredProviders() map[string]bool {
	out := map[string]bool{}
	for name, p := range c.Providers {
		if p.Metered() {
			out[name] = true
		}
	}
	return out
}

// Endpoint describes where a provider points, for display.
func (p Provider) Endpoint() string {
	if p.BaseURL == "" {
		return p.Kind + " default endpoint"
	}
	return p.BaseURL
}

// validateProviders checks the provider and role tables.
func (c Config) validateProviders() error {
	for name, p := range c.Providers {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("a provider has an empty name")
		}
		switch p.Kind {
		case KindAnthropic, KindOpenAI:
		default:
			return fmt.Errorf("provider %q has kind %q, want %q or %q",
				name, p.Kind, KindAnthropic, KindOpenAI)
		}
		if p.BaseURL != "" && !strings.HasPrefix(p.BaseURL, "https://") &&
			!strings.HasPrefix(p.BaseURL, "http://") {
			return fmt.Errorf("provider %q has base_url %q, which is not an http(s) URL", name, p.BaseURL)
		}
		// A key in the file would end up in backups, in version control, and
		// in every process that can read the daemon's data directory.
		if strings.ContainsAny(p.KeyEnv, " \t\n=") {
			return fmt.Errorf("provider %q: key_env %q must be the NAME of an environment variable, not a key",
				name, p.KeyEnv)
		}
		for _, m := range p.Models {
			if strings.TrimSpace(m) == "" {
				return fmt.Errorf("provider %q lists an empty model name", name)
			}
		}
	}

	for _, name := range RoleNames {
		r, ok := c.Roles[name]
		if !ok {
			return fmt.Errorf("role %q is not configured", name)
		}
		want, ok := agentProtocol[r.Agent]
		if !ok {
			return fmt.Errorf("role %q has agent %q, want %q or %q",
				name, r.Agent, AgentClaude, AgentCodex)
		}
		p, ok := c.Providers[r.Provider]
		if !ok {
			return fmt.Errorf("role %q names provider %q, which is not configured", name, r.Provider)
		}
		// The load-bearing check. A role may use either CLI, but the CLI it
		// picks speaks one protocol, so the provider has to speak the same
		// one. Without this the mismatch surfaces as an opaque HTTP error
		// from inside the agent, halfway through a paid-for task.
		if p.Kind != want {
			return fmt.Errorf("role %q runs through %s, which speaks %s, but provider %q is %s",
				name, r.Agent, want, r.Provider, p.Kind)
		}
		if r.Model != "" && len(p.Models) > 0 && !contains(p.Models, r.Model) {
			return fmt.Errorf("role %q asks provider %q for model %q, which is not in its model list",
				name, r.Provider, r.Model)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
