package agent

import "testing"

func TestIsAuthFailure(t *testing.T) {
	auth := []string{
		"not logged in",
		"Please run `claude login`",
		"401 Unauthorized",
		"403 Forbidden",
		"invalid api key",
		"authentication failed",
		"OAuth token has expired",
		"credentials not found",
		// Anthropic's real 401 shape: a JSON body with no space-separated
		// "invalid api key" and no "authentication failed" phrase anywhere in
		// it. Only "authentication_error" (the "type" field) makes this
		// detectable; every other marker in the list is silent on it.
		`{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
	}
	for _, m := range auth {
		if !IsAuthFailure(m) {
			t.Errorf("IsAuthFailure(%q) = false, want true", m)
		}
	}
	notAuth := []string{
		"429 Too Many Requests",
		"invalid_json_schema",
		"step timeout after 30m",
		"unknown flag: --nope",
		"",
		// A routine diagnostic line containing the bare word "login" must not
		// pause the run on its own, even when it precedes an unrelated fatal
		// error later in the same 500-character stderr window.
		"checking login state: OK",
		"checking login state: OK\ndisk full: no space left on device",
	}
	for _, m := range notAuth {
		if IsAuthFailure(m) {
			t.Errorf("IsAuthFailure(%q) = true, want false", m)
		}
	}
}

func TestAuthFailureIsNotRetryable(t *testing.T) {
	// Retrying an auth failure just burns time; the run must pause instead.
	if IsRetryable("not logged in") {
		t.Error("auth failure must not be retryable")
	}
}
