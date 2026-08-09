package store

import (
	"context"
	"fmt"
	"time"
)

// Who said a thing.
const (
	SpeakerOperator  = "operator"
	SpeakerArchitect = "architect"
)

// ArchitectTurn is one turn of the design conversation.
//
// A separate table from steps, which is NOT NULL REFERENCES tasks(id): this
// happens before there is a task, and is the thing that decides what the tasks
// are.
type ArchitectTurn struct {
	ID         int64
	ProposalID int64
	Speaker    string
	Body       string
	// Usage, on the architect's turns only. A conversation costs real agent
	// turns, and the wizard should say so before the operator has had ten.
	CostUSD      float64
	InputTokens  int
	OutputTokens int
	ErrMsg       string
	CreatedAt    time.Time
}

const architectTurnColumns = `id, proposal_id, speaker, body, cost_usd,
	input_tokens, output_tokens, err_msg, created_at`

// AddArchitectTurn appends one turn.
func (s *Store) AddArchitectTurn(ctx context.Context, t ArchitectTurn) (ArchitectTurn, error) {
	t.CreatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO architect_turns (proposal_id, speaker, body, cost_usd,
			input_tokens, output_tokens, err_msg, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		t.ProposalID, t.Speaker, t.Body, t.CostUSD, t.InputTokens,
		t.OutputTokens, t.ErrMsg, t.CreatedAt.Format(rfc3339))
	if err != nil {
		return ArchitectTurn{}, fmt.Errorf("insert architect turn: %w", err)
	}
	if t.ID, err = res.LastInsertId(); err != nil {
		return ArchitectTurn{}, fmt.Errorf("architect turn id: %w", err)
	}
	return t, nil
}

// ArchitectTurns returns one proposal's conversation, oldest first — which is
// the only order a conversation can be read in.
func (s *Store) ArchitectTurns(ctx context.Context, proposalID int64) ([]ArchitectTurn, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+architectTurnColumns+` FROM architect_turns
		 WHERE proposal_id = ? ORDER BY id ASC`, proposalID)
	if err != nil {
		return nil, fmt.Errorf("query architect turns: %w", err)
	}
	defer rows.Close()

	var out []ArchitectTurn
	for rows.Next() {
		var t ArchitectTurn
		var created string
		if err := rows.Scan(&t.ID, &t.ProposalID, &t.Speaker, &t.Body,
			&t.CostUSD, &t.InputTokens, &t.OutputTokens, &t.ErrMsg, &created); err != nil {
			return nil, fmt.Errorf("scan architect turn: %w", err)
		}
		if t.CreatedAt, err = time.Parse(rfc3339, created); err != nil {
			return nil, fmt.Errorf("parse architect turn created_at: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ArchitectSpend totals what one conversation has cost.
func (s *Store) ArchitectSpend(ctx context.Context, proposalID int64) (float64, error) {
	var cost float64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd), 0) FROM architect_turns WHERE proposal_id = ?`,
		proposalID).Scan(&cost)
	if err != nil {
		return 0, fmt.Errorf("architect spend: %w", err)
	}
	return cost, nil
}

// FailStrandedArchitectTurns answers every design conversation left waiting on
// a reply by a daemon restart.
//
// The counterpart to FailStrandedChatTurns, and stranded the same way: a turn
// is a goroutine, not a claimable task, and busy is derived from the operator
// having spoken last — so nothing else would ever clear it and the wizard would
// say "thinking…" for ever.
//
// Only conversations still being designed. Once a proposal has been accepted,
// failed or discarded, its transcript is a record rather than something anyone
// is waiting on, and appending to it would be rewriting history.
func (s *Store) FailStrandedArchitectTurns(ctx context.Context, reason string) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id FROM proposals p
		WHERE p.state = ?
		  AND (SELECT t.speaker FROM architect_turns t
		       WHERE t.proposal_id = p.id ORDER BY t.id DESC LIMIT 1) = ?`,
		ProposalDesigning, SpeakerOperator)
	if err != nil {
		return 0, fmt.Errorf("find stranded design conversations: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan stranded design conversation: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, id := range ids {
		_, err := s.AddArchitectTurn(ctx, ArchitectTurn{
			ProposalID: id, Speaker: SpeakerArchitect,
			Body: "I could not reply: " + reason, ErrMsg: reason,
		})
		if err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}
