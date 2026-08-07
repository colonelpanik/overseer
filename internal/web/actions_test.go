package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/store"
)

func post(t *testing.T, s *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

func TestPostTasksQueuesATask(t *testing.T) {
	s, st := newTestServer(t)
	repo := initRepo(t)

	rec := post(t, s, "/tasks", url.Values{
		"repo": {repo}, "goal": {"Add CSV export"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	tasks, err := st.ListTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Goal != "Add CSV export" {
		t.Fatalf("tasks = %+v", tasks)
	}
	if tasks[0].State != "queued" {
		t.Errorf("State = %q, want queued", tasks[0].State)
	}
}

func TestPostTasksRejectsMissingFields(t *testing.T) {
	s, _ := newTestServer(t)
	for name, form := range map[string]url.Values{
		"no repo": {"goal": {"g"}},
		"no goal": {"repo": {"/tmp"}},
	} {
		if rec := post(t, s, "/tasks", form); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}
}

func TestPostContinueUnparksAnEscalatedTask(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	if _, err := st.CreateTask(ctx, store2Task()); err != nil {
		t.Fatal(err)
	}

	rec := post(t, s, "/task/1/continue", nil)
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
	if got.MaxIterations != 20 {
		t.Errorf("MaxIterations = %d, want 20", got.MaxIterations)
	}
	if got.ErrMsg != "" {
		t.Errorf("ErrMsg = %q, want it cleared", got.ErrMsg)
	}
}

func TestPostAbandonFailsTheTask(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	if _, err := st.CreateTask(ctx, store2Task()); err != nil {
		t.Fatal(err)
	}

	if rec := post(t, s, "/task/1/abandon", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	got, err := st.GetTask(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "failed" {
		t.Errorf("State = %q, want failed", got.State)
	}
}

func TestPostContinueOnNonEscalatedTaskIsRejected(t *testing.T) {
	s, st := newTestServer(t)
	task := store2Task()
	task.State = "planning"
	if _, err := st.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if rec := post(t, s, "/task/1/continue", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestTranscriptEndpointServesTheFile(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	task, err := st.CreateTask(ctx, store2Task())
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"result"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	step, err := st.StartStep(ctx, store.Step{
		TaskID: task.ID, Phase: "plan", Iteration: 1, Agent: "claude",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishStep(ctx, step, nil); err != nil {
		t.Fatal(err)
	}

	rec := get(t, s, "/task/1/transcript/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"type":"result"`) {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestTranscriptEndpointRefusesAStepFromAnotherTask(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	if _, err := st.CreateTask(ctx, store2Task()); err != nil {
		t.Fatal(err)
	}
	other := store2Task()
	other.Slug = "other"
	second, err := st.CreateTask(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	step, err := st.StartStep(ctx, store.Step{
		TaskID: second.ID, Phase: "plan", Iteration: 1, Agent: "claude",
		TranscriptPath: filepath.Join(t.TempDir(), "x.jsonl"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishStep(ctx, step, nil); err != nil {
		t.Fatal(err)
	}

	// Step belongs to task 2; asking for it under task 1 must 404.
	if rec := get(t, s, "/task/1/transcript/1"); rec.Code == http.StatusOK {
		t.Error("transcript from another task was served")
	}
}
