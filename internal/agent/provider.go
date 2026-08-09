package agent

// Provider kinds, duplicated from internal/config on purpose: this package is
// the boundary between overseer and the CLIs, and it should not need the
// daemon's settings types to build an argv and an environment.
const (
	KindAnthropic = "anthropic"
	KindOpenAI    = "openai"
)

// CodexKeyEnv is the variable codex is told to read a third-party credential
// from. Fixed and overseer's own, so an operator's key_env name — or a key set
// inline in the config — reaches codex the same way either way.
const CodexKeyEnv = "OVERSEER_PROVIDER_KEY"

// ProviderEnv returns the environment overrides that point one agent CLI at a
// particular endpoint.
//
// Neither CLI takes an endpoint as a flag, so this is where a provider turns
// into something the process can actually use. The mapping is per (agent,
// protocol) pair and lives in one place so a wrong variable name is one fix
// rather than a hunt:
//
//   - `claude` reads ANTHROPIC_BASE_URL for the endpoint and, when the key
//     lives under a name it does not know, ANTHROPIC_AUTH_TOKEN for the
//     credential. Pointing it at an in-house gateway is exactly these two.
//   - `codex` gets only its credential here. It will not take an endpoint from
//     the environment at all once it has a ChatGPT login: OPENAI_BASE_URL is
//     ignored and the request goes to OpenAI, which then rejects the
//     third-party model name. Its endpoint therefore travels on the command
//     line as a named provider — see CodexArgs — and this sets the variable
//     that provider entry reads the key from.
//
// key is the value read from the provider's key_env, never the variable name:
// this function has no business reading the daemon's environment, and a
// caller that passes the name by mistake gets an obviously wrong credential
// rather than a silently unauthenticated run.
func ProviderEnv(agentName, kind, baseURL, key string) map[string]string {
	env := map[string]string{}

	switch agentName {
	case "claude":
		if kind != KindAnthropic {
			// The caller validated this; returning nothing rather than a
			// half-configured environment keeps the failure loud.
			return env
		}
		if baseURL != "" {
			env["ANTHROPIC_BASE_URL"] = baseURL
		}
		if key != "" {
			// ANTHROPIC_AUTH_TOKEN rather than ANTHROPIC_API_KEY: a gateway
			// in front of an Anthropic-shaped endpoint takes a bearer token,
			// and setting both makes the CLI send two credentials.
			env["ANTHROPIC_AUTH_TOKEN"] = key
		}

	case "codex":
		if kind != KindOpenAI {
			return env
		}
		// Deliberately not OPENAI_BASE_URL. A codex logged in with a ChatGPT
		// account ignores it and talks to OpenAI regardless, so the endpoint
		// goes on the command line as a named provider instead — see
		// CodexOpts. The credential still travels here, under the name that
		// provider entry points at.
		if key != "" {
			env[CodexKeyEnv] = key
			// Kept for a codex configured with an API-key login and no
			// third-party endpoint, where this is the credential it reads.
			env["OPENAI_API_KEY"] = key
		}
	}
	return env
}
