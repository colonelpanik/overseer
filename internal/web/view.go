package web

import (
	"fmt"
	"time"

	"overseer/internal/store"
)

// Badge maps a task state to a colour class.
func Badge(state string) string {
	switch state {
	case "done":
		return "green"
	case "failed":
		return "red"
	case "escalated":
		return "amber"
	case "queued":
		return "grey"
	default:
		return "blue"
	}
}

// Progress renders the phase and iteration counter, which is the fastest
// signal on the board for whether a task is converging or ping-ponging.
func Progress(t store.Task) string {
	if t.Phase == "" {
		return t.State
	}
	return fmt.Sprintf("%s %d/%d", t.Phase, t.Iteration, t.MaxIterations)
}

// TaskCard is one task as shown on the board.
type TaskCard struct {
	Task     store.Task
	Totals   store.Totals
	Badge    string
	Progress string
	Elapsed  string
}

// BoardView is the board page's data.
type BoardView struct {
	Title string
	Tasks []TaskCard
}

// TimelineEntry is one agent step plus the findings it produced.
type TimelineEntry struct {
	Step     store.Step
	Findings []store.Finding
	Duration string
}

// TaskView is the task page's data.
type TaskView struct {
	Title     string
	Task      store.Task
	Totals    store.Totals
	Badge     string
	Progress  string
	Timeline  []TimelineEntry
	PunchList []store.Finding
	TakeOver  string
}

// humanDuration renders a duration compactly for the dashboard.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return "<1s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// elapsed reports how long a task has been going, or how long it took.
func elapsed(t store.Task) string {
	end := time.Now()
	if t.State == "done" || t.State == "failed" {
		end = t.UpdatedAt
	}
	if t.CreatedAt.IsZero() {
		return ""
	}
	return humanDuration(end.Sub(t.CreatedAt))
}

// stepDuration reports how long one step took.
func stepDuration(s store.Step) string {
	if s.StartedAt.IsZero() {
		return ""
	}
	end := s.EndedAt
	if end.IsZero() {
		end = time.Now()
	}
	return humanDuration(end.Sub(s.StartedAt))
}
