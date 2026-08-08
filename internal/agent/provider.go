package agent

// Provider kinds, duplicated from internal/config on purpose: this package is
// the boundary between overseer and the CLIs, and it should not need the
// daemon's settings types to build an argv and an environment.
const (
	KindAnthropic = "anthropic"
	KindOpenAI    = "openai"
)

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
//   - `codex` reads OPENAI_BASE_URL and OPENAI_API_KEY for its built-in
//     OpenAI-compatible provider, which is what any OpenAI-shaped endpoint
//     presents itself as.
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
		if baseURL != "" {
			env["OPENAI_BASE_URL"] = baseURL
		}
		if key != "" {
			env["OPENAI_API_KEY"] = key
		}
	}
	return env
}
