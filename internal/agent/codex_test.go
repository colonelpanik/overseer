package agent

import (
	"slices"
	"strings"
	"testing"
)

func TestParseCodexStreamFixture(t *testing.T) {
	events := parseFixture(t, "../../testdata/codex-stream.jsonl", ParseCodexLine)
	if len(events) != 5 {
		t.Fatalf("parsed %d events, want 5", len(events))
	}

	wantKinds := []EventKind{
		EventInit, EventOther, EventOther, EventMessage, EventResult,
	}
	for i, want := range wantKinds {
		if events[i].Kind != want {
			t.Errorf("event %d kind = %q, want %q", i, events[i].Kind, want)
		}
	}

	const wantThread = "019fd8cb-aab7-7672-9568-eeb98693e79d"
	if events[0].SessionID != wantThread {
		t.Errorf("init SessionID = %q, want %q", events[0].SessionID, wantThread)
	}
	if !strings.Contains(events[3].Text, "changes_requested") {
		t.Errorf("agent_message Text = %q, want the verdict JSON", events[3].Text)
	}
	if events[4].InputTokens != 13487 || events[4].OutputTokens != 20 {
		t.Errorf("result tokens = %d/%d, want 13487/20",
			events[4].InputTokens, events[4].OutputTokens)
	}
}

func TestParseCodexReasoningItemIsNotAMessage(t *testing.T) {
	// Only agent_message items carry the verdict. Treating reasoning as a
	// message would make the last message the wrong text.
	ev, err := ParseCodexLine([]byte(`{"type":"item.completed","item":{"type":"reasoning","text":"hmm"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != EventOther {
		t.Errorf("Kind = %q, want EventOther", ev.Kind)
	}
	if ev.Text != "" {
		t.Errorf("Text = %q, want empty", ev.Text)
	}
}

func TestParseCodexFailedTurnFixture(t *testing.T) {
	events := parseFixture(t, "../../testdata/codex-failed.jsonl", ParseCodexLine)
	if len(events) != 4 {
		t.Fatalf("parsed %d events, want 4", len(events))
	}
	if events[2].Kind != EventError {
		t.Errorf("event 2 kind = %q, want EventError", events[2].Kind)
	}
	if !strings.Contains(events[2].ErrMsg, "invalid_json_schema") {
		t.Errorf("ErrMsg = %q, want it to mention invalid_json_schema", events[2].ErrMsg)
	}
	if events[3].Kind != EventError {
		t.Errorf("turn.failed kind = %q, want EventError", events[3].Kind)
	}
}

func TestParseCodexUnknownTypeIsOther(t *testing.T) {
	ev, err := ParseCodexLine([]byte(`{"type":"item.started","item":{"type":"command_execution"}}`))
	if err != nil {
		t.Fatalf("unknown event types must not error: %v", err)
	}
	if ev.Kind != EventOther {
		t.Errorf("Kind = %q, want EventOther", ev.Kind)
	}
}

func TestCodexArgsAlwaysReadOnlyWithSchema(t *testing.T) {
	args := CodexArgs(CodexOpts{
		Prompt:          "review the diff",
		SchemaPath:      "/tmp/verdict.schema.json",
		LastMessagePath: "/tmp/last.json",
	})
	if args[0] != "exec" {
		t.Errorf("args[0] = %q, want exec", args[0])
	}
	for _, want := range []string{"-s", "read-only", "--json",
		"--output-schema", "/tmp/verdict.schema.json",
		"--output-last-message", "/tmp/last.json", "review the diff"} {
		if !slices.Contains(args, want) {
			t.Errorf("args %v missing %q", args, want)
		}
	}
	if slices.Contains(args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Error("the reviewer must never bypass the sandbox")
	}
}

func TestCodexArgsRoutesAThirdPartyEndpointThroughAModelProvider(t *testing.T) {
	// Environment variables are not enough. A codex logged in with a ChatGPT
	// account ignores OPENAI_BASE_URL entirely and sends the request to OpenAI,
	// which rejects the model name — so the operator sees "not supported when
	// using Codex with a ChatGPT account" and no clue that their gateway was
	// never contacted. A named provider is what actually redirects it.
	args := CodexArgs(CodexOpts{
		Prompt:  "go",
		Model:   "verda/kimi-k3",
		BaseURL: "https://llm.internal.example/v1",
		KeyEnv:  "OVERSEER_PROVIDER_KEY",
		WireAPI: "responses",
	})
	joined := strings.Join(args, " ")

	for _, want := range []string{
		`model_provider="overseer"`,
		`model_providers.overseer.base_url="https://llm.internal.example/v1"`,
		`model_providers.overseer.env_key="OVERSEER_PROVIDER_KEY"`,
		`model_providers.overseer.wire_api="responses"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv is missing %q\ngot: %s", want, joined)
		}
	}
	// The model still goes through -m, and the prompt stays last.
	if !strings.Contains(joined, "-m verda/kimi-k3") {
		t.Errorf("model flag missing: %s", joined)
	}
	if args[len(args)-1] != "go" {
		t.Errorf("prompt is not last: %v", args)
	}
}

func TestCodexArgsLeavesAVendorProviderOnItsOwnLogin(t *testing.T) {
	// No base_url means the vendor's own endpoint, where codex's stored
	// ChatGPT or API-key login is the credential. Emitting a provider override
	// there would replace a working login with one that has no key.
	args := CodexArgs(CodexOpts{Prompt: "go", Model: "gpt-5.6-sol"})
	if joined := strings.Join(args, " "); strings.Contains(joined, "model_provider") {
		t.Errorf("a vendor provider should not be overridden: %s", joined)
	}
}

func TestCodexArgsDefaultsTheWireAPIToResponses(t *testing.T) {
	// codex 0.147 refuses wire_api = "chat" outright, so an unset value must
	// not reach it as one.
	args := CodexArgs(CodexOpts{
		Prompt: "go", BaseURL: "https://llm.internal.example/v1", KeyEnv: "K",
	})
	if !strings.Contains(strings.Join(args, " "), `wire_api="responses"`) {
		t.Errorf("wire_api should default to responses: %v", args)
	}
}
