package agent

import (
	"encoding/json"
	"strings"
)

// transcriptLine covers the parts of both CLIs' JSONL shapes that are worth
// showing live. It is deliberately separate from claudeLine and the Codex
// parser: those exist to drive the loop and must be strict about what they
// accept, whereas this one only has to produce a readable line and may ignore
// anything it does not recognise.
type transcriptLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	Message struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
			// A tool result's payload is a bare string on some turns and a
			// content-block array on others.
			Content json.RawMessage `json:"content"`
		} `json:"content"`
	} `json:"message"`

	// Codex emits a flatter shape.
	Text string `json:"text"`
	Item struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`

	RateLimitInfo struct {
		Status string `json:"status"`
	} `json:"rate_limit_info"`
}

// SummariseTranscript renders the last max events of a JSONL transcript as one
// readable line each, for the dashboard's live pane.
//
// It never fails: a transcript being written to right now can end mid-line,
// and a half-written line is simply skipped rather than reported as corrupt.
func SummariseTranscript(raw []byte, max int) []string {
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if s := summariseLine(line); s != "" {
			out = append(out, s)
		}
	}
	if max > 0 && len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}

func summariseLine(line string) string {
	var tl transcriptLine
	if err := json.Unmarshal([]byte(line), &tl); err != nil {
		return ""
	}

	switch tl.Type {
	case "system":
		// Hook chatter (hook_started, hook_response) shares this type and is
		// the single noisiest thing in a Claude transcript. Only init says
		// anything about the work.
		if tl.Subtype != "init" {
			return ""
		}
		return pad("system") + "init"

	case "assistant":
		var parts []string
		for _, c := range tl.Message.Content {
			switch c.Type {
			case "text":
				if t := firstLine(c.Text); t != "" {
					parts = append(parts, pad("assistant")+t)
				}
			case "tool_use":
				parts = append(parts, pad("tool_use")+strings.TrimSpace(c.Name+" "+toolTarget(c.Input)))
			}
		}
		return strings.Join(parts, "\n")

	case "user":
		// Tool results come back as user turns; the payload is the tool's
		// output, which is where a failing command actually shows up.
		for _, c := range tl.Message.Content {
			if c.Type != "tool_result" {
				continue
			}
			if t := firstLine(c.Text); t != "" {
				return pad("output") + t
			}
			if t := blockText(c.Content); t != "" {
				return pad("output") + t
			}
		}
		return ""

	case "rate_limit_event":
		// "allowed" is the steady state and would be on nearly every line.
		if tl.RateLimitInfo.Status == "" || tl.RateLimitInfo.Status == "allowed" {
			return ""
		}
		return pad("rate_limit") + tl.RateLimitInfo.Status

	case "result":
		return pad("result") + strings.TrimSpace(tl.Subtype)

	case "item.completed", "item.started":
		if t := firstLine(tl.Item.Text); t != "" {
			return pad(tl.Item.Type) + t
		}
		return ""
	}

	if t := firstLine(tl.Text); t != "" {
		return pad(tl.Type) + t
	}
	return ""
}

// toolTarget pulls the most identifying argument out of a tool call's input:
// the path for a file tool, the command for a shell one. Anything else is
// left off rather than dumping a JSON blob into the live pane.
func toolTarget(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var in struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
		Command  string `json:"command"`
		Pattern  string `json:"pattern"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ""
	}
	for _, v := range []string{in.FilePath, in.Path, in.Command, in.Pattern} {
		if v != "" {
			return firstLine(v)
		}
	}
	return ""
}

// blockText reads a tool result's payload, which is either a bare JSON string
// or an array of content blocks.
func blockText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return firstLine(s)
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	for _, b := range blocks {
		if t := firstLine(b.Text); t != "" {
			return t
		}
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	const maxLen = 160
	if len(s) > maxLen {
		s = s[:maxLen-1] + "…"
	}
	return s
}

// pad left-aligns the event kind in a fixed column so the live pane reads as
// two columns rather than ragged prose.
func pad(kind string) string {
	const width = 11
	if len(kind) >= width {
		return kind + " "
	}
	return kind + strings.Repeat(" ", width-len(kind))
}
