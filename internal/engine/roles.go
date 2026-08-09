package engine

import (
	"fmt"
	"os"
	"strings"

	"overseer/internal/agent"
	"overseer/internal/config"
)

// resolved is one role's configuration, flattened into what a run needs.
type resolved struct {
	Role     string
	Agent    string
	Provider string
	Model    string
	// Runner is the CLI driver for this role's agent.
	Runner *agent.Runner
	// Env points that CLI at the provider's endpoint with its credential.
	Env map[string]string
	// Writable is whether this role's agent may write to the worktree. It
	// follows the ROLE, not the agent: the reviewer must not be able to write
	// whichever CLI it happens to run through, and the coder must be able to
	// whichever CLI it happens to run through.
	Writable bool
	// Enforce is whether this role's schema is handed to the CLI to enforce,
	// rather than only stated in its prompt. Follows the provider: it is the
	// endpoint that either supports the constrained tool call or does not.
	Enforce bool
	// BaseURL and WireAPI describe a third-party endpoint, empty for a vendor's
	// own. codex needs them on its command line rather than in its environment
	// — see agent.CodexOpts.
	BaseURL string
	WireAPI string
	// Confined is whether overseer is sandboxing this invocation itself.
	//
	// Both CLIs sandbox their own shell tool with bubblewrap, and a nested user
	// namespace is refused on a kernel that gates them behind an AppArmor
	// profile. When overseer is doing the confining, each agent is told to skip
	// its own; when it is not, the agent's own sandbox is the only one there is
	// and is left alone.
	Confined bool
}

// SetRoles replaces the provider and role tables while the daemon is running,
// which is what the dashboard's settings pane does after writing them to the
// config file.
//
// Only these two tables are reloadable. Everything else in the config —
// listen address, parallelism, sandbox mode — is read at startup and changing
// it mid-run would mean rebuilding things a running task holds references to.
// Roles are read once per agent invocation, so swapping them under the lock is
// safe: a step already in flight keeps the role it resolved.
func (e *Engine) SetRoles(providers map[string]config.Provider, roles map[string]config.Role) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.providers, e.roles = providers, roles
}

// roleConfig returns a Config carrying the live provider and role tables.
func (e *Engine) roleConfig() config.Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	cfg := e.Cfg
	if e.providers != nil {
		cfg.Providers = e.providers
	}
	if e.roles != nil {
		cfg.Roles = e.roles
	}
	return cfg
}

// Roles exposes the live tables, for the dashboard's settings pane.
func (e *Engine) Roles() (map[string]config.Provider, map[string]config.Role) {
	cfg := e.roleConfig()
	return cfg.Providers, cfg.Roles
}

// roleWrites says which roles need a writable worktree.
var roleWrites = map[string]bool{
	config.RoleCode:    true,
	config.RoleReview:  false,
	config.RoleAnalyse: false,
}

// resolveRole turns a role name into the runner, model and environment that
// run it.
//
// A missing credential is an error here rather than an agent failure later:
// the CLI would report it as an authentication problem, which pauses the
// whole run, and the operator would be left guessing which of several
// providers was the one that had no key.
func (e *Engine) resolveRole(name string) (resolved, error) {
	role, provider, err := e.roleConfig().Resolve(name)
	if err != nil {
		return resolved{}, err
	}

	// From the config file or from the environment, whichever holds it.
	key := provider.Credential()
	// An empty credential is only a problem when the provider is a custom
	// endpoint. Against the vendor default the CLI may be logged in through
	// its own stored credentials, which is the common case.
	if key == "" && provider.BaseURL != "" {
		where := "set `key:` on the provider, or name the environment variable in `key_env:`"
		if provider.KeyEnv != "" {
			where = fmt.Sprintf("%s is not set in the daemon's environment", provider.KeyEnv)
		}
		return resolved{}, fmt.Errorf("role %q uses provider %q, which has no credential: %s",
			name, role.Provider, where)
	}

	r := resolved{
		Role:     name,
		Agent:    role.Agent,
		Provider: role.Provider,
		Model:    role.Model,
		BaseURL:  provider.BaseURL,
		WireAPI:  provider.WireAPI,
		Enforce:  provider.StructuredOutput(),
		Env:      agent.ProviderEnv(role.Agent, provider.Kind, provider.BaseURL, key),
		Writable: roleWrites[name],
		Confined: e.confining(),
	}
	switch role.Agent {
	case config.AgentCodex:
		r.Runner = e.Codex
	default:
		r.Runner = e.Claude
	}
	return r, nil
}

// confining reports whether overseer is sandboxing the agents itself.
func (e *Engine) confining() bool {
	return e.Sandbox != nil && e.Sandbox.Name() != "off"
}

// agentEnv is the environment one agent invocation gets: the role's provider
// settings, plus a statement that it is already confined.
//
// The agent CLIs sandbox their own shell tool, and a sandbox inside a sandbox
// is refused on a kernel that only permits unprivileged user namespaces
// through an AppArmor profile — Ubuntu 24.04 and later. Overseer's own sandbox
// works; the agent's then fails on every run with "bwrap: No permissions to
// create a new namespace", which in a transcript reads exactly like overseer's
// sandbox being broken. Telling the agent it is already confined stops it
// trying.
//
// This is the claude half. codex takes a flag rather than an environment
// variable — see CodexArgs — but the rule is the same, and both read it off
// resolved.Confined.
//
// Only when overseer is actually confining it. With the sandbox off, the
// agent's own is the only one there is, and claiming otherwise would turn a
// deliberate "no sandbox" into a silently unprotected agent that believes it
// is protected.
func (e *Engine) agentEnv(role resolved) map[string]string {
	if !role.Confined {
		return role.Env
	}
	env := make(map[string]string, len(role.Env)+1)
	for k, v := range role.Env {
		env[k] = v
	}
	env["CLAUDE_CODE_SANDBOXED"] = "1"
	return env
}

// args builds the argv for one turn of this role.
//
// The two CLIs take different flags for the same job, so the role's agent —
// not the role's name — decides the shape. schemaPath and lastMessage are
// only meaningful for a reviewer; a coder passes them empty.
func (r resolved) args(prompt, resume, schemaPath, lastMessage, schema string) []string {
	if r.Agent == config.AgentCodex {
		return agent.CodexArgs(agent.CodexOpts{
			Prompt:              prompt,
			SchemaPath:          schemaPath,
			LastMessagePath:     lastMessage,
			Model:               r.Model,
			ExternallySandboxed: r.Confined,
			// A third-party endpoint reaches codex as a named provider on the
			// command line, not as OPENAI_BASE_URL — see CodexOpts.
			BaseURL: r.BaseURL,
			KeyEnv:  agent.CodexKeyEnv,
			WireAPI: r.WireAPI,
		})
	}
	opts := agent.ClaudeOpts{
		Prompt:          prompt,
		ResumeSessionID: resume,
		Model:           r.Model,
	}
	if r.structured() {
		// Inline rather than a path: the flag takes the schema itself.
		opts.JSONSchema = schema
	}
	return agent.ClaudeArgs(opts)
}

// structured reports whether this role's CLI will be told to emit a JSON
// object matching a schema, rather than merely asked to in its prompt.
//
// Both CLIs can: `codex exec` takes --output-schema, and `claude -p` takes
// --json-schema, which it implements as a forced tool call whose input_schema
// is the schema. An earlier version of this said claude could not, which was
// true of the version it was written against and stopped being true without
// anything here noticing — so a claude-backed reviewer or analysis was getting
// the contract as prose when it could have been getting it enforced.
//
// The provider decides, because it is the endpoint that either supports the
// constrained tool call or does not: a gateway that mishandles it would fail
// every run, and structured_output: false is the way back.
//
// ParseVerdict and ParseProposal remain the guarantee either way. Enforcement
// makes malformed output far less likely; it is the strict parser, not the
// flag, that stands between it and a silent approval.
func (r resolved) structured() bool { return r.Enforce }

// withInlineSchema appends the verdict schema to a review prompt.
//
// It is used when the reviewer's CLI has no --output-schema flag. The schema
// stops being enforced by the tool and becomes documentation for the model,
// exactly as it is for the analysis prompt — and exactly as there, the
// guarantee moves to the parser on the other side.
func withInlineSchema(prompt string) string {
	return prompt + "\n\nYour reply must be this JSON object and nothing else — " +
		"no prose before it, no explanation after it, no markdown fence:\n\n" +
		strings.TrimSpace(string(agent.VerdictSchema))
}

// reviewOutput reads a review's verdict, preferring the file the CLI was
// asked to write and falling back to its final message.
//
// The fallback is not a convenience: a reviewer running through a CLI with no
// --output-last-message flag has no file at all, and one that does can still
// fail to write it. Either way, no verdict means no approval — the caller
// treats an unparseable result as a failure, never as agreement.
func reviewOutput(lastPath, finalText string) []byte {
	if lastPath != "" {
		if raw, err := os.ReadFile(lastPath); err == nil {
			return raw
		}
	}
	return []byte(finalText)
}
