package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ClaudeOpts describes one `claude -p` invocation.
type ClaudeOpts struct {
	Prompt string
	// ResumeSessionID continues an existing session, which is how a Codex
	// review reaches the same Claude session that produced the work.
	ResumeSessionID string
	Model           string
	// JSONSchema constrains the final answer, as schema text rather than a
	// path — the flag takes it inline. Claude Code implements it as a forced
	// tool call whose input_schema is this, so a reply of the wrong shape
	// cannot be produced rather than being caught afterwards by the parser.
	JSONSchema string
}

// ClaudeArgs builds the argv for a headless Claude run.
//
// --verbose is mandatory: without it --output-format stream-json is
// rejected.
//
// --add-dir is not passed, but do not mistake that for confinement.
// bypassPermissions skips the permission system entirely, and --add-dir only
// extends that system's allow-list, so omitting it grants nothing and
// restricts nothing. Left to itself, the process would run as the daemon's
// user with that user's full filesystem access; the worktree would be only
// its working directory. Real confinement comes from the OS-level sandbox in
// internal/sandbox (see Engine.sandboxSpec), which wraps this argv with
// bubblewrap before it ever runs — this function only builds the CLI's own
// flags and knows nothing about that layer. A previous version of this
// comment claimed overseer did not set up an OS-level sandbox at all; that
// stopped being true once the sandbox package was added, and the project has
// already been burned once by a stale containment claim surviving in
// documentation after the code changed under it.
func ClaudeArgs(o ClaudeOpts) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "bypassPermissions",
	}
	if o.ResumeSessionID != "" {
		args = append(args, "--resume", o.ResumeSessionID)
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.JSONSchema != "" {
		args = append(args, "--json-schema", o.JSONSchema)
	}
	return append(args, o.Prompt)
}

type claudeLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`

	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`

	// StructuredOutput is the validated object, present only when the run was
	// given --json-schema. An object rather than a string: the sibling
	// "result" field holds the same thing as text, but only when the CLI
	// chose to serialise it there.
	StructuredOutput json.RawMessage `json:"structured_output"`

	IsError    bool    `json:"is_error"`
	TotalCost  float64 `json:"total_cost_usd"`
	StopReason string  `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`

	RateLimitInfo struct {
		Status        string `json:"status"`
		RateLimitType string `json:"rateLimitType"`
	} `json:"rate_limit_info"`
}

// ParseClaudeLine normalises one line of `claude -p --output-format
// stream-json` output. Unrecognised event types yield EventOther; only
// malformed JSON is an error.
func ParseClaudeLine(line []byte) (Event, error) {
	var cl claudeLine
	if err := json.Unmarshal(line, &cl); err != nil {
		return Event{}, fmt.Errorf("parse claude event: %w", err)
	}
	ev := Event{Kind: EventOther, SessionID: cl.SessionID, Raw: string(line)}

	switch cl.Type {
	case "system":
		// Hook events (hook_started, hook_response) share this type and are
		// ignored; only init carries information we need.
		if cl.Subtype == "init" {
			ev.Kind = EventInit
		}

	case "assistant":
		ev.Kind = EventMessage
		var sb strings.Builder
		for _, c := range cl.Message.Content {
			if c.Type == "text" {
				sb.WriteString(c.Text)
			}
		}
		ev.Text = sb.String()

	case "rate_limit_event":
		ev.Kind = EventRateLimit
		if cl.RateLimitInfo.Status != "" && cl.RateLimitInfo.Status != "allowed" {
			ev.ErrMsg = fmt.Sprintf("rate limit %s (%s)",
				cl.RateLimitInfo.Status, cl.RateLimitInfo.RateLimitType)
		}

	case "result":
		ev.Kind = EventResult
		if len(cl.StructuredOutput) > 0 && string(cl.StructuredOutput) != "null" {
			ev.Structured = string(cl.StructuredOutput)
		}
		ev.CostUSD = cl.TotalCost
		ev.InputTokens = cl.Usage.InputTokens
		ev.OutputTokens = cl.Usage.OutputTokens
		if cl.IsError || (cl.Subtype != "" && cl.Subtype != "success") {
			ev.ErrMsg = fmt.Sprintf("claude result %s (stop_reason %s)",
				cl.Subtype, cl.StopReason)
		}
	}
	return ev, nil
}
