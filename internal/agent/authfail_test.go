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
