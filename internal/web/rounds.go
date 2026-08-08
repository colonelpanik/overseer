package web

import (
	"fmt"
	"sort"

	"overseer/internal/store"
)

// A round is one review: a Codex review or a verify run, together with what it
// objected to. Rounds are the unit the dashboard measures convergence in,
// because they are the only steps that produce a yes-or-no answer — a Claude
// turn always "succeeds", so counting those would draw a flat line whether the
// task was converging or going in circles.
type round struct {
	Phase string
	Iter  int
	Agent string
	// Label is the compact "plan i2" form used on axes and in finding
	// lifetimes.
	Label string
	// Blocking counts the findings that held the loop up.
	Blocking int
	// Summaries are the blocking findings' summaries, which is exactly what
	// the engine fingerprints for oscillation detection.
	Summaries []string
}

// roundsOf adapts the store's review rounds to the shape the charts want.
// This is the only path: the board and the detail read the same rows, so a
// task's sparkline and its convergence chart cannot end up telling different
// stories about the same run.
func roundsOf(rows []store.ReviewRound) []round {
	out := make([]round, 0, len(rows))
	for _, r := range rows {
		out = append(out, round{
			Phase:     r.Phase,
			Iter:      r.Iteration,
			Agent:     r.Agent,
			Label:     fmt.Sprintf("%s i%d", r.Phase, r.Iteration),
			Blocking:  len(r.Blocking),
			Summaries: r.Blocking,
		})
	}
	return out
}

// Bar is one column of the convergence chart or a task row's sparkline.
type Bar struct {
	// Height is in pixels, already scaled against the tallest bar.
	Height int
	// Zero marks a round that raised nothing — drawn hollow, because a
	// converged round reading as a short filled bar is the single most
	// misleading thing this chart could do.
	Zero bool
	// Alert marks a round whose findings had been seen before.
	Alert bool
	Label string
	Title string
}

// bars scales a run of rounds to a pixel height, keeping at most last columns
// (zero or fewer keeps all of them).
func bars(rs []round, last, minPx, maxPx int) []Bar {
	if last > 0 && len(rs) > last {
		rs = rs[len(rs)-last:]
	}
	max := 1
	for _, r := range rs {
		if r.Blocking > max {
			max = r.Blocking
		}
	}
	repeats := repeatRounds(rs)

	out := make([]Bar, 0, len(rs))
	for i, r := range rs {
		b := Bar{
			Zero:  r.Blocking == 0,
			Alert: repeats[i],
			Label: fmt.Sprintf("i%d", r.Iter),
			Title: fmt.Sprintf("%s · %d blocking", r.Label, r.Blocking),
		}
		if b.Zero {
			b.Height = 3
		} else {
			b.Height = minPx + (r.Blocking*(maxPx-minPx))/max
		}
		out = append(out, b)
	}
	return out
}

// repeatRounds marks the rounds that raised a finding an earlier round had
// already raised. That recurrence is the oscillation signal, and it is what
// the chart colours rather than "lots of findings", which is a normal first
// round.
func repeatRounds(rs []round) map[int]bool {
	seen := map[string]bool{}
	out := map[int]bool{}
	for i, r := range rs {
		for _, s := range r.Summaries {
			if seen[s] {
				out[i] = true
			}
		}
		for _, s := range r.Summaries {
			seen[s] = true
		}
	}
	return out
}

// MatrixCell values.
const (
	CellNew    = "new"
	CellRepeat = "repeat"
	CellAbsent = "absent"
)

// MatrixRow is one finding's presence across the recent rounds.
type MatrixRow struct {
	Label string
	Cells []string
}

// Matrix is the oscillation fingerprint: findings down the side, recent rounds
// across the top. It answers the question the iteration counter cannot — is
// this task making progress, or handing the same objection back and forth.
type Matrix struct {
	Iters []string
	Rows  []MatrixRow
	Note  string
}

// matrixWidth is how many recent rounds the fingerprint shows. Six is enough
// to see a cycle and narrow enough that each column stays legible.
const matrixWidth = 6

// matrixRows caps the table so one pathological task cannot push the timeline
// off the screen.
const matrixRows = 6

// fingerprint builds the oscillation matrix, or returns nil when nothing has
// recurred and the plain convergence chart tells the whole story.
func fingerprint(rs []round) *Matrix {
	if len(rs) < 2 {
		return nil
	}
	window := rs
	if len(window) > matrixWidth {
		window = window[len(window)-matrixWidth:]
	}

	// first records where each summary was seen for the first time across the
	// whole task, not just the window: a finding raised at i1 and again at i9
	// is a repeat, even though i1 is off the left edge.
	first := map[string]int{}
	for i, r := range rs {
		for _, s := range r.Summaries {
			if _, ok := first[s]; !ok {
				first[s] = i
			}
		}
	}
	offset := len(rs) - len(window)

	// Build a row per summary appearing anywhere in the window, then keep the
	// matrix only if some cell is a repeat.
	//
	// Repeat-ness is judged against the whole run, not the window. A finding
	// raised at i1 and again at i8 appears once inside a six-round window, and
	// counting occurrences per window would call that "new" and drop the
	// matrix entirely — which is the exact case the matrix exists to show.
	type row struct {
		summary string
		cells   []string
		repeats int
		first   int
	}
	var rows []row
	seen := map[string]bool{}
	total := 0
	for _, r := range window {
		for _, s := range r.Summaries {
			if seen[s] {
				continue
			}
			seen[s] = true
			cur := row{summary: s, first: first[s]}
			for wi, w := range window {
				cell := CellAbsent
				if containsString(w.Summaries, s) {
					cell = CellNew
					if first[s] < offset+wi {
						cell = CellRepeat
						cur.repeats++
					}
				}
				cur.cells = append(cur.cells, cell)
			}
			total += cur.repeats
			rows = append(rows, cur)
		}
	}
	if total == 0 {
		return nil
	}

	// The finding actually stuck in the loop is the top row.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].repeats != rows[j].repeats {
			return rows[i].repeats > rows[j].repeats
		}
		if rows[i].first != rows[j].first {
			return rows[i].first < rows[j].first
		}
		return rows[i].summary < rows[j].summary
	})
	if len(rows) > matrixRows {
		rows = rows[:matrixRows]
	}

	m := &Matrix{}
	for _, r := range window {
		m.Iters = append(m.Iters, fmt.Sprintf("i%d", r.Iter))
	}
	for _, r := range rows {
		m.Rows = append(m.Rows, MatrixRow{Label: r.summary, Cells: r.cells})
	}
	m.Note = fmt.Sprintf("A filled red cell is a finding this review had already raised before. "+
		"The worst one has come back %s — that is what the oscillation check trips on, "+
		"and raising the blocking threshold is usually what clears it.", times(rows[0].repeats))
	return m
}

func times(n int) string {
	if n == 1 {
		return "once"
	}
	return fmt.Sprintf("%d times", n)
}

// LedgerRow is one finding's whole life on a task.
type LedgerRow struct {
	Severity string
	Summary  string
	Loc      string
	// Track has one cell per round: "raised", "absent" or "resolved".
	Track []string
	Life  string
	Open  bool
}

// Track cell values.
const (
	TrackRaised   = "raised"
	TrackAbsent   = "absent"
	TrackResolved = "resolved"
)

// ledger lists every finding ever raised on a task, deduplicated by summary
// and annotated with when it appeared and whether it is still open.
//
// A finding stays on the list after it is fixed. That is the point: a repeat
// that scrolled off the timeline three iterations ago looks brand new
// otherwise, and "we have seen this before" is the single most useful thing
// this page can tell an operator staring at a parked task.
func ledger(steps []store.Step, byStep map[int64][]store.Finding, rs []round) []LedgerRow {
	// Track every finding, blocking or not, but measure lifetime against the
	// review rounds so a punch-list nit still shows when it first appeared.
	type entry struct {
		f       store.Finding
		rounds  map[int]bool
		firstAt int
		lastAt  int
		order   int
	}
	byText := map[string]*entry{}
	var order int

	ri := -1
	for _, s := range steps {
		if s.Verdict == "" {
			continue
		}
		ri++
		for _, f := range byStep[s.ID] {
			e, ok := byText[f.Summary]
			if !ok {
				e = &entry{f: f, rounds: map[int]bool{}, firstAt: ri, order: order}
				order++
				byText[f.Summary] = e
			}
			e.rounds[ri] = true
			e.lastAt = ri
		}
	}

	out := make([]LedgerRow, 0, len(byText))
	for _, e := range byText {
		row := LedgerRow{
			Severity: e.f.Severity,
			Summary:  e.f.Summary,
			Loc:      location(e.f),
			Open:     e.lastAt == len(rs)-1,
		}
		for i := range rs {
			switch {
			case e.rounds[i]:
				row.Track = append(row.Track, TrackRaised)
			case i > e.lastAt:
				row.Track = append(row.Track, TrackResolved)
			default:
				row.Track = append(row.Track, TrackAbsent)
			}
		}
		if row.Open {
			row.Life = "open since " + rs[e.firstAt].Label
		} else if e.lastAt+1 < len(rs) {
			row.Life = "resolved by " + rs[e.lastAt+1].Label
		} else {
			row.Life = "last seen " + rs[e.lastAt].Label
		}
		out = append(out, row)
	}

	// Open findings first — they are the ones that still need something — then
	// by the order they first appeared.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Open != out[j].Open {
			return out[i].Open
		}
		return byText[out[i].Summary].order < byText[out[j].Summary].order
	})
	return out
}

// location renders a finding's file:line, or the agent-free label a verify
// failure gets, which has no file at all.
func location(f store.Finding) string {
	if f.File == "" {
		return ""
	}
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	return f.File
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
