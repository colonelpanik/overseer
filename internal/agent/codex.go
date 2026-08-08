package agent

import (
	"encoding/json"
	"fmt"
)

// CodexOpts describes one `codex exec` invocation.
type CodexOpts struct {
	Prompt string
	// SchemaPath constrains the final message to a JSON Schema. The schema
	// is validated in OpenAI strict mode, where every key in a properties
	// object must also appear in that object's required array.
	SchemaPath      string
	LastMessagePath string
	Model           string
}

// CodexArgs builds the argv for a headless Codex review.
//
// -s read-only is always passed: the reviewer must never be able to write.
// The caller must additionally set the process's stdin to /dev/null, because
// codex exec appends piped stdin to the prompt and blocks waiting for EOF.
func CodexArgs(o CodexOpts) []string {
	args := []string{"exec", "-s", "read-only", "--json"}
	if o.SchemaPath != "" {
		args = append(args, "--output-schema", o.SchemaPath)
	}
	if o.LastMessagePath != "" {
		args = append(args, "--output-last-message", o.LastMessagePath)
	}
	if o.Model != "" {
		args = append(args, "-m", o.Model)
	}
	return append(args, o.Prompt)
}

type codexLine struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`

	Item struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`

	Error struct {
		Message string `json:"message"`
	} `json:"error"`

	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// ParseCodexLine normalises one line of `codex exec --json` output.
// Unrecognised event and item types yield EventOther; only malformed JSON
// is an error.
func ParseCodexLine(line []byte) (Event, error) {
	var cl codexLine
	if err := json.Unmarshal(line, &cl); err != nil {
		return Event{}, fmt.Errorf("parse codex event: %w", err)
	}
	ev := Event{Kind: EventOther, Raw: string(line)}

	switch cl.Type {
	case "thread.started":
		ev.Kind = EventInit
		ev.SessionID = cl.ThreadID

	case "item.completed":
		// Reasoning, command_execution and file_change items also arrive
		// here; only agent_message carries the verdict.
		if cl.Item.Type == "agent_message" {
			ev.Kind = EventMessage
			ev.Text = cl.Item.Text
		}

	case "turn.completed":
		ev.Kind = EventResult
		ev.InputTokens = cl.Usage.InputTokens
		ev.OutputTokens = cl.Usage.OutputTokens

	case "error":
		ev.Kind = EventError
		ev.ErrMsg = cl.Message

	case "turn.failed":
		ev.Kind = EventError
		ev.ErrMsg = cl.Error.Message
		if ev.ErrMsg == "" {
			ev.ErrMsg = "codex turn failed"
		}
	}
	return ev, nil
}
