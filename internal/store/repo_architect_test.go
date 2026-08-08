package store

import (
	"context"
	"testing"
)

func newProposal(t *testing.T, s *Store, kind string) Proposal {
	t.Helper()
	p, err := s.CreateProposal(context.Background(), Proposal{
		RepoPath: "/src/widget", State: ProposalDesigning, Kind: kind,
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	return p
}

// A conversation only reads in one order, and the store has to preserve it.
func TestArchitectTurnsComeBackInOrder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p := newProposal(t, s, ProposalCreate)

	said := []struct{ speaker, body string }{
		{SpeakerOperator, "a CLI that syncs S3 buckets, Go, no dependencies"},
		{SpeakerArchitect, "Two questions before I sketch this."},
		{SpeakerOperator, "one-way sync, and yes it needs resume"},
		{SpeakerArchitect, "Here is the shape."},
	}
	for _, turn := range said {
		if _, err := s.AddArchitectTurn(ctx, ArchitectTurn{
			ProposalID: p.ID, Speaker: turn.speaker, Body: turn.body, CostUSD: 0.25,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.ArchitectTurns(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(said) {
		t.Fatalf("got %d turns, want %d", len(got), len(said))
	}
	for i, turn := range said {
		if got[i].Speaker != turn.speaker || got[i].Body != turn.body {
			t.Errorf("turn %d = %s: %q, want %s: %q",
				i, got[i].Speaker, got[i].Body, turn.speaker, turn.body)
		}
	}

	// The wizard says what the conversation has cost, so it has to add up.
	spend, err := s.ArchitectSpend(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diff := spend - 1.0; diff < -0.001 || diff > 0.001 {
		t.Errorf("spend = %v, want 1.00", spend)
	}
}

func TestArchitectTurnsAreScopedToTheirProposal(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mine := newProposal(t, s, ProposalCreate)
	theirs := newProposal(t, s, ProposalCreate)

	for _, id := range []int64{mine.ID, theirs.ID} {
		if _, err := s.AddArchitectTurn(ctx, ArchitectTurn{
			ProposalID: id, Speaker: SpeakerOperator, Body: "hello",
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ArchitectTurns(ctx, mine.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("got %d turns, want only this proposal's", len(got))
	}
}

// Deleting a proposal takes its conversation with it rather than leaving turns
// pointing at nothing.
func TestArchitectTurnsCascade(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p := newProposal(t, s, ProposalCreate)

	if _, err := s.AddArchitectTurn(ctx, ArchitectTurn{
		ProposalID: p.ID, Speaker: SpeakerOperator, Body: "hello",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `DELETE FROM proposals WHERE id = ?`, p.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ArchitectTurns(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("%d orphaned turns survived their proposal", len(got))
	}
}

// The kind, the design and the session have to survive a round trip, and an
// existing proposal must read back as an analysis.
func TestProposalKindDesignAndSessionRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	p := newProposal(t, s, ProposalCreate)
	p.Design = "# Design\n\nOne binary, one SQLite file.\n"
	p.ArchitectSession = "sess-1"
	if err := s.SaveProposal(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetProposal(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != ProposalCreate {
		t.Errorf("Kind = %q, want create", got.Kind)
	}
	if got.Design != p.Design || got.ArchitectSession != "sess-1" {
		t.Errorf("design or session lost: %+v", got)
	}

	// A proposal created without a kind is an analysis, which is what every
	// row written before the column existed is.
	plain, err := s.CreateProposal(ctx, Proposal{RepoPath: "/r", State: ProposalDraft})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Kind != ProposalAnalyse {
		t.Errorf("Kind = %q, want analyse by default", plain.Kind)
	}
}
