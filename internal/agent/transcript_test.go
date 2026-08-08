package agent

import (
	"strings"
	"testing"
)

func TestSummariseTranscriptDropsHookChatter(t *testing.T) {
	// Hook events share the "system" type and outnumber everything else in a
	// real Claude transcript. Left in, they push the actual work off screen.
	raw := []byte(`{"type":"system","subtype":"hook_started","hook_name":"SessionStart"}
{"type":"system","subtype":"hook_response","output":"ADHD MODE ACTIVE"}
{"type":"system","subtype":"init","session_id":"abc"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Reading the findings."}]}}
`)
	got := SummariseTranscript(raw, 0)
	if len(got) != 2 {
		t.Fatalf("lines = %d (%v), want the init and the message only", len(got), got)
	}
	if !strings.HasPrefix(got[0], "system") || !strings.Contains(got[0], "init") {
		t.Errorf("line 0 = %q", got[0])
	}
	if !strings.Contains(got[1], "Reading the findings.") {
		t.Errorf("line 1 = %q", got[1])
	}
}

func TestSummariseTranscriptNamesTheToolAndItsTarget(t *testing.T) {
	raw := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"internal/web/export.go"}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}
`)
	got := SummariseTranscript(raw, 0)
	if len(got) != 2 {
		t.Fatalf("lines = %v", got)
	}
	if !strings.Contains(got[0], "Edit internal/web/export.go") {
		t.Errorf("line 0 = %q, want the file path", got[0])
	}
	if !strings.Contains(got[1], "Bash go test ./...") {
		t.Errorf("line 1 = %q, want the command", got[1])
	}
}

func TestSummariseTranscriptReadsAToolResultInEitherShape(t *testing.T) {
	// Tool results come back as a bare string on some turns and as a content
	// block array on others; a failing command has to be visible either way.
	raw := []byte(`{"type":"user","message":{"content":[{"type":"tool_result","content":"FAIL\tinternal/web"}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"ok  \tinternal/store"}]}]}}
`)
	got := SummariseTranscript(raw, 0)
	if len(got) != 2 {
		t.Fatalf("lines = %v", got)
	}
	if !strings.Contains(got[0], "FAIL") {
		t.Errorf("line 0 = %q", got[0])
	}
	if !strings.Contains(got[1], "internal/store") {
		t.Errorf("line 1 = %q", got[1])
	}
}

func TestSummariseTranscriptSkipsAHalfWrittenLine(t *testing.T) {
	// The live pane tails a file the agent is still writing to, so the last
	// line is regularly incomplete. That must not fail the whole render.
	raw := []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"complete"}]}}
{"type":"assistant","message":{"conte`)
	got := SummariseTranscript(raw, 0)
	if len(got) != 1 || !strings.Contains(got[0], "complete") {
		t.Errorf("lines = %v, want only the complete event", got)
	}
}

func TestSummariseTranscriptKeepsTheMostRecentEvents(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		sb.WriteString(`{"type":"assistant","message":{"content":[{"type":"text","text":"line ` + string(rune('a'+i%26)) + `"}]}}` + "\n")
	}
	got := SummariseTranscript([]byte(sb.String()), 5)
	if len(got) != 5 {
		t.Fatalf("lines = %d, want 5", len(got))
	}
	// 50 lines, so the last is index 49 -> 'a'+49%26 = 'x'.
	if !strings.Contains(got[4], "line x") {
		t.Errorf("last line = %q, want the newest event", got[4])
	}
}

func TestSummariseTranscriptHandlesCodexEvents(t *testing.T) {
	raw := []byte(`{"type":"thread.started","thread_id":"019f"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"reasoning","text":"considering the diff"}}
{"type":"turn.completed","usage":{"input_tokens":13487}}
`)
	got := SummariseTranscript(raw, 0)
	if len(got) != 1 {
		t.Fatalf("lines = %v, want only the reasoning item", got)
	}
	if !strings.Contains(got[0], "considering the diff") {
		t.Errorf("line = %q", got[0])
	}
}

func TestSummariseTranscriptTruncatesALongLine(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := SummariseTranscript([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"`+long+`"}]}}`), 0)
	if len(got) != 1 {
		t.Fatalf("lines = %v", got)
	}
	if len(got[0]) > 200 {
		t.Errorf("line length = %d, want it bounded", len(got[0]))
	}
	if !strings.HasSuffix(got[0], "…") {
		t.Errorf("line = %q, want an ellipsis marking the truncation", got[0])
	}
}
