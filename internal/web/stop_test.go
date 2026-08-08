package web

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"overseer/internal/engine"
	"overseer/internal/store"
)

// stoppableTask returns a task parked mid-plan with a real worktree, which is
// the state the plan tab and the stop controls are for.
func stoppableTask(t *testing.T, s *Server, st *store.Store) store.Task {
	t.Helper()
	ctx := context.Background()

	dir := initRepo(t)
	task, err := s.eng.Submit(ctx, engine.BatchTask{Repo: dir, Goal: "a task to stop"})
	if err != nil {
		t.Fatal(err)
	}
	// A real git repository, because editing the plan commits it — and a
	// worktree that is not one is exactly what the empty-directory guard in
	// internal/worktree refuses.
	wt := dir
	task.State = "planning"
	task.Phase = "plan"
	task.Iteration = 1
	task.WorktreeDir = wt
	if err := st.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "PLAN.md"), []byte("# the plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestBoardOffersStopWhileRunningAndStartWhileStopped(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	task := stoppableTask(t, s, st)
	path := "/task/" + strconv.FormatInt(task.ID, 10)

	body := get(t, s, path).Body.String()
	if !strings.Contains(body, path+"/stop") {
		t.Error("no Stop control on a running task")
	}
	// The escalation is offered too, but as its own control rather than as the
	// default: a soft stop wastes nothing.
	if !strings.Contains(body, `name="now" value="1"`) {
		t.Error("no Stop now escalation offered")
	}
	if strings.Contains(body, path+"/start") {
		t.Error("Start offered on a task that is not stopped")
	}

	if err := st.StopTask(ctx, task.ID, true); err != nil {
		t.Fatal(err)
	}
	body = get(t, s, path).Body.String()
	if !strings.Contains(body, path+"/start") {
		t.Error("no Start control on a stopped task")
	}
	// Stopped reads as a state even though it is a column, because to an
	// operator that is what it is.
	if !strings.Contains(body, "stopped") {
		t.Error("the board does not say the task is stopped")
	}
}

func TestPostStopAndStartRoundTrip(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	task := stoppableTask(t, s, st)
	id := strconv.FormatInt(task.ID, 10)

	if rec := post(t, s, "/task/"+id+"/stop", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("stop = %d: %s", rec.Code, rec.Body.String())
	}
	got, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Stopped() {
		t.Fatal("the task is not stopped")
	}
	if got.State != "planning" {
		t.Errorf("State = %q; a stop must not overwrite where the task had got to", got.State)
	}

	if rec := post(t, s, "/task/"+id+"/start", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("start = %d: %s", rec.Code, rec.Body.String())
	}
	got, err = st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stopped() {
		t.Error("the task is still stopped after start")
	}
}

// The plan tab is read-only while the task runs, because a write would race
// the agent editing the same tree.
func TestPlanTabIsReadOnlyUntilTheTaskIsStopped(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	task := stoppableTask(t, s, st)
	path := "/task/" + strconv.FormatInt(task.ID, 10) + "?tab=plan"

	body := get(t, s, path).Body.String()
	if !strings.Contains(body, "the plan") {
		t.Error("the plan tab does not show the plan")
	}
	if strings.Contains(body, `name="plan"`) {
		t.Error("the plan is editable while the task is running")
	}
	if !strings.Contains(body, "Stop the task to edit") {
		t.Error("the pane does not say why it is read-only")
	}

	if err := st.StopTask(ctx, task.ID, true); err != nil {
		t.Fatal(err)
	}
	body = get(t, s, path).Body.String()
	if !strings.Contains(body, `name="plan"`) {
		t.Error("the plan is not editable once the task is stopped")
	}
}

func TestPostPlanWritesAndRefusesWhileRunning(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	task := stoppableTask(t, s, st)
	id := strconv.FormatInt(task.ID, 10)

	// Running: refused.
	rec := post(t, s, "/task/"+id+"/plan", url.Values{"plan": {"# too early\n"}})
	if rec.Code != http.StatusConflict {
		t.Errorf("plan write while running = %d, want 409", rec.Code)
	}

	if err := st.StopTask(ctx, task.ID, true); err != nil {
		t.Fatal(err)
	}
	rec = post(t, s, "/task/"+id+"/plan", url.Values{"plan": {"# the operator's plan\n"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("plan write while stopped = %d: %s", rec.Code, rec.Body.String())
	}
	// Back to the tab it was edited on, not the default.
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "tab=plan") {
		t.Errorf("Location = %q, want the plan tab", loc)
	}

	body, err := os.ReadFile(filepath.Join(task.WorktreeDir, "PLAN.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "the operator's plan") {
		t.Errorf("PLAN.md = %q, want the edit", string(body))
	}
	// The exec session is cleared, which is what makes the next turn re-read
	// the file rather than run on what the old session remembers.
	got, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExecSessionID != "" {
		t.Error("the exec session survived the plan edit")
	}
}

func TestStopAllPersistsAndCanBeCleared(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()

	if rec := post(t, s, "/stopall", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("stopall = %d: %s", rec.Code, rec.Body.String())
	}
	// Persisted, so a restart does not quietly resume everything.
	reason, err := st.Setting(ctx, store.SettingStopAll)
	if err != nil {
		t.Fatal(err)
	}
	if reason == "" {
		t.Fatal("the global stop was not persisted")
	}
	if s.eng.PauseReason() == "" {
		t.Error("the run is not actually stopped")
	}

	body := get(t, s, "/").Body.String()
	if !strings.Contains(body, "Run stopped") {
		t.Error("the banner does not distinguish an operator stop from an auth pause")
	}
	if strings.Contains(body, "authentication failure") {
		t.Error("an operator stop is reported as an authentication failure")
	}

	if rec := post(t, s, "/stopall", url.Values{"clear": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("clear = %d: %s", rec.Code, rec.Body.String())
	}
	if reason, _ = st.Setting(ctx, store.SettingStopAll); reason != "" {
		t.Error("the global stop was not cleared")
	}
	if s.eng.PauseReason() != "" {
		t.Error("the run is still stopped after clearing")
	}
}

func TestEveryStopRouteRequiresSameOrigin(t *testing.T) {
	s, st := newTestServer(t)
	task := stoppableTask(t, s, st)
	id := strconv.FormatInt(task.ID, 10)

	for _, path := range []string{
		"/task/" + id + "/stop",
		"/task/" + id + "/start",
		"/task/" + id + "/restart",
		"/task/" + id + "/plan",
		"/stopall",
	} {
		rec := crossSitePost(t, s, path, url.Values{"plan": {"x"}})
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s from another site = %d, want 403", path, rec.Code)
		}
	}
}
