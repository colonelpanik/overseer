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

	var key string
	if provider.KeyEnv != "" {
		key = os.Getenv(provider.KeyEnv)
		// An empty key is only a problem when the provider is a custom
		// endpoint. Against the vendor default the CLI may be logged in
		// through its own stored credentials, which is the common case.
		if key == "" && provider.BaseURL != "" {
			return resolved{}, fmt.Errorf(
				"role %q uses provider %q, whose key_env %s is not set in the daemon's environment",
				name, role.Provider, provider.KeyEnv)
		}
	}

	r := resolved{
		Role:     name,
		Agent:    role.Agent,
		Provider: role.Provider,
		Model:    role.Model,
		Env:      agent.ProviderEnv(role.Agent, provider.Kind, provider.BaseURL, key),
		Writable: roleWrites[name],
	}
	switch role.Agent {
	case config.AgentCodex:
		r.Runner = e.Codex
	default:
		r.Runner = e.Claude
	}
	return r, nil
}

// args builds the argv for one turn of this role.
//
// The two CLIs take different flags for the same job, so the role's agent —
// not the role's name — decides the shape. schemaPath and lastMessage are
// only meaningful for a reviewer; a coder passes them empty.
func (r resolved) args(prompt, resume, schemaPath, lastMessage string) []string {
	if r.Agent == config.AgentCodex {
		return agent.CodexArgs(agent.CodexOpts{
			Prompt:          prompt,
			SchemaPath:      schemaPath,
			LastMessagePath: lastMessage,
			Model:           r.Model,
		})
	}
	return agent.ClaudeArgs(agent.ClaudeOpts{
		Prompt:          prompt,
		ResumeSessionID: resume,
		Model:           r.Model,
	})
}

// structured reports whether this role's CLI can be told to emit a JSON
// object matching a schema.
//
// Only `codex exec` takes --output-schema. A reviewer running through
// `claude` therefore has the contract stated in its prompt instead, and the
// verdict is read from its final message. ParseVerdict is the guarantee
// either way, which is why swapping the reviewer's CLI is safe: the strict
// parser, not the flag, is what stands between malformed output and a silent
// approval.
func (r resolved) structured() bool { return r.Agent == config.AgentCodex }

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
