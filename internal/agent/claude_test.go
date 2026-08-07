package agent

import (
	"bufio"
	"os"
	"slices"
	"strings"
	"testing"
)

func parseFixture(t *testing.T, path string, fn func([]byte) (Event, error)) []Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	var events []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		ev, err := fn([]byte(line))
		if err != nil {
			t.Fatalf("parse %q: %v", line[:min(60, len(line))], err)
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return events
}

func TestParseClaudeStreamFixture(t *testing.T) {
	events := parseFixture(t, "../../testdata/claude-stream.jsonl", ParseClaudeLine)
	if len(events) != 6 {
		t.Fatalf("parsed %d events, want 6", len(events))
	}

	wantKinds := []EventKind{
		EventOther, EventOther, EventInit, EventMessage, EventRateLimit, EventResult,
	}
	for i, want := range wantKinds {
		if events[i].Kind != want {
			t.Errorf("event %d kind = %q, want %q", i, events[i].Kind, want)
		}
	}

	const wantSession = "39c24ced-54f6-46c5-b6e8-115d13185c95"
	if events[2].SessionID != wantSession {
		t.Errorf("init SessionID = %q, want %q", events[2].SessionID, wantSession)
	}
	if events[3].Text != "Wrote PLAN.md" {
		t.Errorf("assistant Text = %q, want \"Wrote PLAN.md\"", events[3].Text)
	}

	res := events[5]
	if res.CostUSD < 0.138 || res.CostUSD > 0.1382 {
		t.Errorf("result CostUSD = %v, want ~0.1381", res.CostUSD)
	}
	if res.InputTokens != 2 || res.OutputTokens != 4 {
		t.Errorf("result tokens = %d/%d, want 2/4", res.InputTokens, res.OutputTokens)
	}
	if res.ErrMsg != "" {
		t.Errorf("successful result has ErrMsg %q", res.ErrMsg)
	}
	// The recorded fixture's rate_limit_event carries status "allowed" — the
	// one case that must NOT populate ErrMsg. Without this assertion, the
	// "allowed" half of the not-equal-to-"allowed" check is never exercised
	// by the fixture at all.
	if rl := events[4]; rl.Kind == EventRateLimit && rl.ErrMsg != "" {
		t.Errorf("allowed rate-limit event has ErrMsg %q, want empty", rl.ErrMsg)
	}
}

func TestParseClaudeErrorResult(t *testing.T) {
	line := `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}`
	ev, err := ParseClaudeLine([]byte(line))
	if err != nil {
		t.Fatalf("ParseClaudeLine: %v", err)
	}
	if ev.Kind != EventResult {
		t.Errorf("Kind = %q, want EventResult", ev.Kind)
	}
	if ev.ErrMsg == "" {
		t.Error("is_error result must populate ErrMsg")
	}
}

// TestParseClaudeErrorResultIsErrorAlone proves the OR half of "is_error OR
// subtype != success" that TestParseClaudeErrorResult cannot: it sets
// is_error true together with the "success" subtype, so an accidental `&&`
// (which TestParseClaudeErrorResult alone would not catch, since it sets
// both bad conditions at once) would wrongly leave ErrMsg empty here.
func TestParseClaudeErrorResultIsErrorAlone(t *testing.T) {
	line := `{"type":"result","subtype":"success","is_error":true,"session_id":"s1","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}`
	ev, err := ParseClaudeLine([]byte(line))
	if err != nil {
		t.Fatalf("ParseClaudeLine: %v", err)
	}
	if ev.ErrMsg == "" {
		t.Error("is_error:true must populate ErrMsg even with subtype \"success\"")
	}
}

// TestParseClaudeErrorResultSubtypeAlone is the other OR half: is_error is
// false but the subtype is not "success". An accidental `&&` would also
// wrongly leave ErrMsg empty here.
func TestParseClaudeErrorResultSubtypeAlone(t *testing.T) {
	line := `{"type":"result","subtype":"error_during_execution","is_error":false,"session_id":"s1","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1}}`
	ev, err := ParseClaudeLine([]byte(line))
	if err != nil {
		t.Fatalf("ParseClaudeLine: %v", err)
	}
	if ev.ErrMsg == "" {
		t.Error("subtype != \"success\" must populate ErrMsg even with is_error:false")
	}
}

func TestParseClaudeRateLimitExhausted(t *testing.T) {
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour"},"session_id":"s1"}`
	ev, err := ParseClaudeLine([]byte(line))
	if err != nil {
		t.Fatalf("ParseClaudeLine: %v", err)
	}
	if ev.Kind != EventRateLimit {
		t.Fatalf("Kind = %q, want EventRateLimit", ev.Kind)
	}
	if ev.ErrMsg == "" {
		t.Error("non-allowed rate limit status must populate ErrMsg")
	}
}

func TestParseClaudeRateLimitAllowed(t *testing.T) {
	// The other half of the not-equal-to-"allowed" check, isolated from the
	// fixture: an explicit "allowed" status must leave ErrMsg empty.
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour"},"session_id":"s1"}`
	ev, err := ParseClaudeLine([]byte(line))
	if err != nil {
		t.Fatalf("ParseClaudeLine: %v", err)
	}
	if ev.Kind != EventRateLimit {
		t.Fatalf("Kind = %q, want EventRateLimit", ev.Kind)
	}
	if ev.ErrMsg != "" {
		t.Errorf("allowed rate limit status has ErrMsg %q, want empty", ev.ErrMsg)
	}
}

func TestParseClaudeUnknownTypeIsOtherNotError(t *testing.T) {
	ev, err := ParseClaudeLine([]byte(`{"type":"something_new_in_a_future_release","x":1}`))
	if err != nil {
		t.Fatalf("unknown event types must not error: %v", err)
	}
	if ev.Kind != EventOther {
		t.Errorf("Kind = %q, want EventOther", ev.Kind)
	}
}

func TestParseClaudeMalformedJSONErrors(t *testing.T) {
	if _, err := ParseClaudeLine([]byte(`{"type":`)); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestParseClaudeConcatenatesMultipleTextBlocks(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"one "},{"type":"tool_use","name":"Bash"},{"type":"text","text":"two"}]},"session_id":"s1"}`
	ev, err := ParseClaudeLine([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Text != "one two" {
		t.Errorf("Text = %q, want \"one two\"", ev.Text)
	}
}

func TestClaudeArgsFreshSession(t *testing.T) {
	args := ClaudeArgs(ClaudeOpts{Prompt: "do the thing"})
	for _, want := range []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions", "do the thing",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("args %v missing %q", args, want)
		}
	}
	if slices.Contains(args, "--resume") {
		t.Error("fresh session must not pass --resume")
	}
	if slices.Contains(args, "--add-dir") {
		t.Error("--add-dir is pointless under bypassPermissions and misleads about scoping")
	}
}

func TestClaudeArgsResume(t *testing.T) {
	args := ClaudeArgs(ClaudeOpts{Prompt: "here is the review", ResumeSessionID: "sess-9"})
	i := slices.Index(args, "--resume")
	if i < 0 {
		t.Fatalf("args %v missing --resume", args)
	}
	if args[i+1] != "sess-9" {
		t.Errorf("--resume value = %q, want sess-9", args[i+1])
	}
}
