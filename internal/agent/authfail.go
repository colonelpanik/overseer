package agent

import "strings"

// authMarkers are substrings that indicate the agent CLI is not
// authenticated.
//
// The HTTP status codes are matched together with their canonical reason
// phrase ("401 unauthorized", "403 forbidden"), not as bare digits, for the
// same reason retryableMarkers in runner.go pairs "429" with "too many
// requests": a bare "401" or "403" is a common substring of unrelated
// numbers — a token count ("output_tokens":14033), a byte count ("wrote
// 40133 bytes"), an exit code, a PR or issue number ("PR #401") — and
// matching it alone would misclassify an ordinary failure as an
// authentication failure, pausing the whole run for no reason. The bare
// words "unauthorized" and "forbidden" are kept unpaired because they are
// whole, distinctive English words with no such collision risk, and already
// cover a plain "401 Unauthorized" / "403 Forbidden" message on their own.
//
// The bare word "login" is deliberately NOT in this list, even though an
// explicit instruction to log in is: a routine diagnostic line like
// "checking login state: OK", printed ahead of an unrelated fatal error,
// would otherwise pause the whole run on a false signal. "claude login" and
// "codex login" are the phrases an agent actually prints when it wants the
// operator to authenticate, and neither collides with incidental text the
// way the bare word does.
//
// "authentication_error" and "invalid_api_key" are the literal values
// Anthropic's API puts in the JSON error body's "type" and code fields
// (`{"error":{"type":"authentication_error","message":"invalid
// x-api-key"}}`) — distinctive machine tokens, not prose, so they are kept
// bare like "invalid_api_key" already was. "invalid x-api-key" covers the
// hyphenated key name the message text actually uses; the older "invalid
// api key" (space-separated) is kept alongside it for CLIs that phrase it
// that way instead.
var authMarkers = []string{
	"not logged in", "claude login", "codex login",
	"401 unauthorized", "403 forbidden",
	"unauthorized", "forbidden",
	"invalid api key", "invalid_api_key", "invalid x-api-key",
	"authentication_error", "authentication failed",
	"token has expired", "credentials not found",
}

// IsAuthFailure reports whether an agent error means the CLI is not
// authenticated. Every task would fail identically, so the engine pauses the
// whole run rather than draining the queue.
func IsAuthFailure(msg string) bool {
	if msg == "" {
		return false
	}
	l := strings.ToLower(msg)
	for _, m := range authMarkers {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}
