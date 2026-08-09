// Package agent drives the Claude and Codex CLIs as subprocesses and
// normalises their JSONL event streams into a single vocabulary. It is the
// only package in overseer that knows a CLI exists.
package agent

// EventKind classifies a normalised event from either agent.
type EventKind string

const (
	// EventInit carries the session or thread identifier needed to resume.
	EventInit EventKind = "init"
	// EventMessage is assistant-visible text.
	EventMessage EventKind = "message"
	// EventResult ends a run and carries usage totals.
	EventResult EventKind = "result"
	// EventError is a fatal error reported inside the stream.
	EventError EventKind = "error"
	// EventRateLimit reports rate-limit status.
	EventRateLimit EventKind = "rate_limit"
	// EventOther is anything we deliberately ignore: hook chatter,
	// reasoning items, turn boundaries, and event types added by future
	// CLI releases. Never an error.
	EventOther EventKind = "other"
)

// Event is one normalised line from an agent's output stream.
type Event struct {
	Kind         EventKind
	SessionID    string
	Text         string
	ErrMsg       string
	CostUSD      float64
	InputTokens  int
	OutputTokens int
	// Raw is the original JSON line, written verbatim to the transcript.
	Raw string
}
