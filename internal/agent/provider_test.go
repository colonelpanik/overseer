package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/sandbox"
)

func TestProviderEnvPointsEachCLIAtItsEndpoint(t *testing.T) {
	claude := ProviderEnv("claude", KindAnthropic, "https://gw.internal/v1", "tok")
	if claude["ANTHROPIC_BASE_URL"] != "https://gw.internal/v1" {
		t.Errorf("claude env = %v", claude)
	}
	// A gateway in front of an Anthropic-shaped endpoint takes a bearer
	// token; setting the API key as well would send two credentials.
	if claude["ANTHROPIC_AUTH_TOKEN"] != "tok" {
		t.Errorf("claude env = %v, want the auth token", claude)
	}
	if _, ok := claude["ANTHROPIC_API_KEY"]; ok {
		t.Errorf("claude env = %v, want no second credential", claude)
	}

	codex := ProviderEnv("codex", KindOpenAI, "https://llm.dc.internal/v1", "sk-x")
	if codex["OPENAI_BASE_URL"] != "https://llm.dc.internal/v1" || codex["OPENAI_API_KEY"] != "sk-x" {
		t.Errorf("codex env = %v", codex)
	}
}

func TestProviderEnvLeavesTheVendorDefaultAlone(t *testing.T) {
	// No base URL means the CLI's own default, and no key means its own
	// stored login. Setting either to an empty string would override a
	// working configuration with nothing.
	env := ProviderEnv("claude", KindAnthropic, "", "")
	if len(env) != 0 {
		t.Errorf("env = %v, want empty", env)
	}
}

func TestProviderEnvRefusesAMismatchedProtocol(t *testing.T) {
	// Config validation catches this first; returning nothing rather than a
	// half-configured environment keeps a slip loud instead of producing a
	// CLI pointed at an endpoint it cannot speak to.
	if env := ProviderEnv("claude", KindOpenAI, "https://x/v1", "k"); len(env) != 0 {
		t.Errorf("env = %v, want empty", env)
	}
	if env := ProviderEnv("codex", KindAnthropic, "https://x/v1", "k"); len(env) != 0 {
		t.Errorf("env = %v, want empty", env)
	}
}

// envScript writes a fake agent that prints one environment variable, so a
// test can see what actually reached the child process.
func envScript(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent")
	body := "#!/bin/sh\nprintf '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"%s\"}]}}\\n' \"$" + name + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunSpecEnvReachesAnUnsandboxedAgent(t *testing.T) {
	// With `sandbox: off` the passthrough wrapper hands the argv straight to
	// exec and never touches the environment, so a configured provider would
	// silently have no effect unless the runner layers it on itself.
	r := NewClaudeRunner(envScript(t, "OPENAI_BASE_URL"))
	res, err := r.Run(t.Context(), RunSpec{
		TranscriptPath: filepath.Join(t.TempDir(), "t.jsonl"),
		Sandbox:        sandbox.Passthrough{},
		Env:            map[string]string{"OPENAI_BASE_URL": "https://llm.dc.internal/v1"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.FinalText, "https://llm.dc.internal/v1") {
		t.Errorf("the agent saw %q, want the override", res.FinalText)
	}
}

func TestRunSpecEnvMergesIntoTheSandboxSpecWithoutMutatingIt(t *testing.T) {
	// The spec's map belongs to the caller and is reused across runs; one
	// task's credentials must not end up in another's spec.
	original := sandbox.Spec{Env: map[string]string{"KEEP": "yes"}}
	r := NewClaudeRunner(envScript(t, "KEEP"))
	_, err := r.Run(t.Context(), RunSpec{
		TranscriptPath: filepath.Join(t.TempDir(), "t.jsonl"),
		Sandbox:        sandbox.Passthrough{},
		SandboxSpec:    original,
		Env:            map[string]string{"OPENAI_API_KEY": "sk-secret"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, leaked := original.Env["OPENAI_API_KEY"]; leaked {
		t.Error("the run mutated the caller's sandbox spec")
	}
	if original.Env["KEEP"] != "yes" {
		t.Error("the run disturbed the caller's existing env")
	}
}
