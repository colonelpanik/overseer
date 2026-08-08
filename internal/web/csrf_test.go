package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"overseer/internal/store"
)

// crossSitePost issues a POST carrying Sec-Fetch-Site: cross-site, exactly as
// a browser sends on a form auto-submitted by a hostile third-party page, but
// with a Host header that DOES match the server — isolating the
// Sec-Fetch-Site check from the separate Host check exercised below.
func crossSitePost(t *testing.T, s *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Host = s.cfg.ListenAddr
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

// foreignHostPost issues a same-site POST (no Sec-Fetch-Site problem) whose
// Host header names something other than the configured listener — standing
// in for DNS rebinding, where a hostile page has the browser resolve an
// attacker-controlled name to 127.0.0.1 but cannot forge the Host header that
// goes out with it.
func foreignHostPost(t *testing.T, s *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "evil.example:7777"
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

func TestStateChangingRoutesRejectCrossSiteRequests(t *testing.T) {
	for _, route := range []string{"/tasks", "/task/1/continue", "/task/1/abandon", "/resume"} {
		s, _ := newTestServer(t)
		if rec := crossSitePost(t, s, route, url.Values{"repo": {"/x"}, "goal": {"g"}}); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403 for a cross-site request", route, rec.Code)
		}
	}
}

func TestStateChangingRoutesRejectForeignHost(t *testing.T) {
	for _, route := range []string{"/tasks", "/task/1/continue", "/task/1/abandon", "/resume"} {
		s, _ := newTestServer(t)
		if rec := foreignHostPost(t, s, route, url.Values{"repo": {"/x"}, "goal": {"g"}}); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403 for a foreign Host header", route, rec.Code)
		}
	}
}

func TestCrossSitePostTasksHasNoSideEffect(t *testing.T) {
	s, st := newTestServer(t)
	repo := initRepo(t)

	crossSitePost(t, s, "/tasks", url.Values{"repo": {repo}, "goal": {"Add CSV export"}})

	tasks, err := st.ListTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("a cross-site POST queued a task: %+v", tasks)
	}
}

func TestCrossSitePostContinueHasNoSideEffect(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	if _, err := st.CreateTask(ctx, store2Task()); err != nil {
		t.Fatal(err)
	}

	crossSitePost(t, s, "/task/1/continue", nil)

	got, err := st.GetTask(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "escalated" {
		t.Errorf("State = %q, a cross-site POST must not un-park the task", got.State)
	}
}

func TestCrossSitePostAbandonHasNoSideEffect(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	if _, err := st.CreateTask(ctx, store2Task()); err != nil {
		t.Fatal(err)
	}

	crossSitePost(t, s, "/task/1/abandon", nil)

	got, err := st.GetTask(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "escalated" {
		t.Errorf("State = %q, a cross-site POST must not abandon the task", got.State)
	}
}

func TestCrossSitePostResumeHasNoSideEffect(t *testing.T) {
	s, _ := newTestServer(t)
	s.eng.Pause("no credentials")

	crossSitePost(t, s, "/resume", nil)

	if s.eng.PauseReason() == "" {
		t.Error("a cross-site POST cleared the pause")
	}
}

// TestSameOriginPostStillWorks pins down the non-regression side of F1: a
// request that looks like it came from the dashboard itself — matching Host,
// and either no Sec-Fetch-Site header or an explicit same-origin — must still
// go through and redirect, exactly as before the guard was added.
func TestSameOriginPostStillWorks(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	if _, err := st.CreateTask(ctx, store2Task()); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/task/1/continue", nil)
	req.Host = s.cfg.ListenAddr
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	got, err := st.GetTask(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "planning" {
		t.Errorf("State = %q, want planning", got.State)
	}
}

func TestGetRoutesAreUnaffectedByTheOriginGuard(t *testing.T) {
	// GET routes carry no state-changing side effect, so they are explicitly
	// out of scope for the guard even from a cross-site request.
	s, st := newTestServer(t)
	if _, err := st.CreateTask(context.Background(), store.Task{
		Slug: "t", RepoPath: "/r", Goal: "g", State: "queued",
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Host = "evil.example:7777"
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / with a hostile Sec-Fetch-Site/Host = %d, want 200 (GET routes are unaffected)", rec.Code)
	}
}
