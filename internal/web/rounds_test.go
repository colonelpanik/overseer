package web

import (
	"strings"
	"testing"

	"overseer/internal/store"
)

// reviews builds review rounds from a list of per-round blocking summaries.
func reviews(perRound ...[]string) []round {
	var rows []store.ReviewRound
	for i, summaries := range perRound {
		rows = append(rows, store.ReviewRound{
			Phase: "exec", Iteration: i + 1, Agent: "codex", Blocking: summaries,
		})
	}
	return roundsOf(rows)
}

func TestBarsDrawAConvergedRoundHollow(t *testing.T) {
	// A round that raised nothing rendering as a short filled bar would read
	// as "nearly there" instead of "done", which is the opposite of the truth.
	got := bars(reviews([]string{"a", "b"}, []string{"a"}, nil), 0, 8, 52)
	if len(got) != 3 {
		t.Fatalf("bars = %d, want 3", len(got))
	}
	if got[0].Zero || got[1].Zero {
		t.Error("rounds with findings must not be hollow")
	}
	if !got[2].Zero {
		t.Error("a round that raised nothing must be hollow")
	}
	if got[0].Height <= got[1].Height {
		t.Errorf("heights = %d, %d; two findings must be taller than one",
			got[0].Height, got[1].Height)
	}
}

func TestBarsFlagTheRoundThatRepeatedItself(t *testing.T) {
	// The alert is for recurrence, not volume: a big first round is normal,
	// and the same finding coming back is not.
	got := bars(reviews([]string{"a", "b", "c"}, []string{"d"}, []string{"a"}), 0, 8, 52)
	if got[0].Alert {
		t.Error("the first round cannot be a repeat of anything")
	}
	if got[1].Alert {
		t.Error("a round raising something new is not a repeat")
	}
	if !got[2].Alert {
		t.Error("a round re-raising an earlier finding must be flagged")
	}
}

func TestBarsKeepOnlyTheMostRecentColumns(t *testing.T) {
	got := bars(reviews(nil, nil, nil, nil, nil, nil, nil, []string{"x"}), 6, 4, 15)
	if len(got) != 6 {
		t.Fatalf("bars = %d, want the last 6", len(got))
	}
	if got[5].Zero {
		t.Error("the last column should be the most recent round, which raised something")
	}
}

func TestFingerprintOnlyAppearsWhenSomethingRecurs(t *testing.T) {
	// The matrix costs a lot of vertical space. A task working through a
	// different finding each round has nothing to explain, and the plain
	// convergence chart says it better.
	if fingerprint(reviews([]string{"a"}, []string{"b"}, []string{"c"})) != nil {
		t.Error("no repeat means no fingerprint matrix")
	}
	if fingerprint(reviews([]string{"a"})) != nil {
		t.Error("a single round cannot show a cycle")
	}
	if fingerprint(reviews([]string{"a"}, []string{"a"})) == nil {
		t.Error("a repeated finding must produce the matrix")
	}
}

func TestFingerprintMarksTheFirstAppearanceNewAndLaterOnesRepeat(t *testing.T) {
	m := fingerprint(reviews([]string{"stuck"}, nil, []string{"stuck"}, []string{"stuck"}))
	if m == nil {
		t.Fatal("expected a matrix")
	}
	if len(m.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(m.Rows))
	}
	want := []string{CellNew, CellAbsent, CellRepeat, CellRepeat}
	for i, w := range want {
		if m.Rows[i%len(m.Rows)].Cells[i] != w {
			t.Errorf("cell %d = %q, want %q", i, m.Rows[0].Cells[i], w)
		}
	}
	// Raised at i1, back at i3 and i4: it has come back twice.
	if !strings.Contains(m.Note, "2 times") {
		t.Errorf("note = %q, want it to count the recurrences", m.Note)
	}
}

func TestFingerprintCallsAFindingARepeatEvenWhenItsFirstRoundScrolledOff(t *testing.T) {
	// The window is six rounds wide. A finding raised at i1 and again at i8 is
	// a repeat; labelling it "new" because i1 is off the left edge would hide
	// exactly the case the matrix exists for.
	rs := reviews(
		[]string{"old"}, nil, nil, nil, nil, nil, nil, []string{"old"},
	)
	m := fingerprint(rs)
	if m == nil {
		t.Fatal("expected a matrix")
	}
	last := m.Rows[0].Cells[len(m.Rows[0].Cells)-1]
	if last != CellRepeat {
		t.Errorf("last cell = %q, want %q", last, CellRepeat)
	}
}

// ledgerFixture builds steps and findings for the ledger tests.
func ledgerFixture(perRound [][]store.Finding) ([]store.Step, map[int64][]store.Finding, []round) {
	var steps []store.Step
	byStep := map[int64][]store.Finding{}
	var rows []store.ReviewRound
	for i, findings := range perRound {
		id := int64(i + 1)
		steps = append(steps, store.Step{
			ID: id, Phase: "exec", Iteration: i + 1, Agent: "codex", Verdict: "reviewed",
		})
		byStep[id] = findings
		var blocking []string
		for _, f := range findings {
			if f.Blocking {
				blocking = append(blocking, f.Summary)
			}
		}
		rows = append(rows, store.ReviewRound{
			Phase: "exec", Iteration: i + 1, Agent: "codex", Blocking: blocking,
		})
	}
	return steps, byStep, roundsOf(rows)
}

func TestLedgerKeepsAResolvedFindingAndDatesIt(t *testing.T) {
	steps, byStep, rs := ledgerFixture([][]store.Finding{
		{{Severity: "major", Summary: "fixed one", File: "a.go", Line: 4, Blocking: true}},
		{},
	})
	got := ledger(steps, byStep, rs)
	if len(got) != 1 {
		t.Fatalf("rows = %d, want the resolved finding kept", len(got))
	}
	if got[0].Open {
		t.Error("a finding absent from the last round is resolved")
	}
	if got[0].Life != "resolved by exec i2" {
		t.Errorf("life = %q, want \"resolved by exec i2\"", got[0].Life)
	}
	if got[0].Loc != "a.go:4" {
		t.Errorf("loc = %q, want a.go:4", got[0].Loc)
	}
	want := []string{TrackRaised, TrackResolved}
	for i, w := range want {
		if got[0].Track[i] != w {
			t.Errorf("track[%d] = %q, want %q", i, got[0].Track[i], w)
		}
	}
}

func TestLedgerSortsOpenFindingsFirst(t *testing.T) {
	steps, byStep, rs := ledgerFixture([][]store.Finding{
		{{Severity: "nit", Summary: "went away", Blocking: true}},
		{{Severity: "major", Summary: "still here", Blocking: true}},
	})
	got := ledger(steps, byStep, rs)
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2", len(got))
	}
	if got[0].Summary != "still here" || !got[0].Open {
		t.Errorf("first row = %+v, want the open finding", got[0])
	}
	if got[1].Summary != "went away" || got[1].Open {
		t.Errorf("second row = %+v, want the resolved finding", got[1])
	}
	if got[0].Life != "open since exec i2" {
		t.Errorf("life = %q, want \"open since exec i2\"", got[0].Life)
	}
}

func TestLedgerIncludesFindingsBelowTheBlockingThreshold(t *testing.T) {
	// A punch-list nit never held the loop up, but it is still something
	// somebody said about this change, and it has nowhere else to appear.
	steps, byStep, rs := ledgerFixture([][]store.Finding{
		{{Severity: "nit", Summary: "not worth blocking on", Blocking: false}},
	})
	got := ledger(steps, byStep, rs)
	if len(got) != 1 || got[0].Summary != "not worth blocking on" {
		t.Errorf("ledger = %+v, want the non-blocking finding", got)
	}
}
