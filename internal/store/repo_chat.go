package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SpeakerAssistant is the agent's side of a repository chat. The architect has
// its own constant because it is a different conversation with a different job,
// and a transcript that mixed them would be readable but wrong.
const SpeakerAssistant = "assistant"

// Chat is a durable conversation about one repository.
//
// It has no spend columns: what a conversation cost is the sum of its turns,
// and a stored total could only ever disagree with them.
type Chat struct {
	ID     int64
	RepoID int64
	// Session is the agent session each turn resumes into. Empty when the
	// conversation has not started, or when a resume failed and the next turn
	// must re-seed from the stored turns.
	Session string
	// ArchivedAt is set when the operator started a fresh chat here. An
	// archived chat is readable and refuses new turns.
	ArchivedAt time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Archived reports whether this conversation has been retired.
func (c Chat) Archived() bool { return !c.ArchivedAt.IsZero() }

// ChatTurn is one thing said in a repository chat.
type ChatTurn struct {
	ID     int64
	ChatID int64
	// Speaker is SpeakerOperator or SpeakerAssistant.
	Speaker string
	Body    string
	CostUSD float64
	// InputTokens and OutputTokens are the assistant's usage for this turn.
	InputTokens, OutputTokens int
	// Provider is the configured provider that served the turn, so metered and
	// subscription-covered usage stay distinguishable afterwards.
	Provider string
	// ErrMsg is why a turn produced no answer. The turn is still recorded: the
	// conversation is still there and the operator can ask again.
	ErrMsg    string
	CreatedAt time.Time
}

const chatColumns = `id, repo_id, session, archived_at, created_at, updated_at`

const chatTurnColumns = `id, chat_id, speaker, body, cost_usd, input_tokens,
	output_tokens, provider, err_msg, created_at`

// CreateChat opens a conversation about a repository.
//
// A repository may only have one live chat, and that is a unique index rather
// than a check here: two simultaneous clicks would otherwise both find no chat
// and both create one.
func (s *Store) CreateChat(ctx context.Context, repoID int64) (Chat, error) {
	if repoID == 0 {
		return Chat{}, errors.New("a chat needs a repository")
	}
	now := time.Now().UTC()
	c := Chat{RepoID: repoID, CreatedAt: now, UpdatedAt: now}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO chats (repo_id, session, archived_at, created_at, updated_at)
		VALUES (?,?,?,?,?)`,
		c.RepoID, "", "", now.Format(rfc3339), now.Format(rfc3339))
	if err != nil {
		return Chat{}, fmt.Errorf("insert chat: %w", err)
	}
	if c.ID, err = res.LastInsertId(); err != nil {
		return Chat{}, fmt.Errorf("chat id: %w", err)
	}
	return c, nil
}

func scanChat(sc interface{ Scan(...any) error }) (Chat, error) {
	var c Chat
	var archived, created, updated string
	if err := sc.Scan(&c.ID, &c.RepoID, &c.Session, &archived, &created, &updated); err != nil {
		return Chat{}, err
	}
	var err error
	if archived != "" {
		if c.ArchivedAt, err = time.Parse(rfc3339, archived); err != nil {
			return Chat{}, fmt.Errorf("parse chat archived_at: %w", err)
		}
	}
	if c.CreatedAt, err = time.Parse(rfc3339, created); err != nil {
		return Chat{}, fmt.Errorf("parse chat created_at: %w", err)
	}
	if c.UpdatedAt, err = time.Parse(rfc3339, updated); err != nil {
		return Chat{}, fmt.Errorf("parse chat updated_at: %w", err)
	}
	return c, nil
}

// GetChat loads one conversation by ID.
func (s *Store) GetChat(ctx context.Context, id int64) (Chat, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+chatColumns+` FROM chats WHERE id = ?`, id)
	c, err := scanChat(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Chat{}, fmt.Errorf("chat %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Chat{}, fmt.Errorf("get chat: %w", err)
	}
	return c, nil
}

// LiveChat returns a repository's current conversation, or ErrNotFound.
//
// It never creates one. The dashboard calls this on every render, and a lookup
// that wrote would mean opening the overlay started a conversation nobody asked
// for — against every repository the operator happened to look at.
func (s *Store) LiveChat(ctx context.Context, repoID int64) (Chat, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+chatColumns+` FROM chats WHERE repo_id = ? AND archived_at = ''`, repoID)
	c, err := scanChat(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Chat{}, fmt.Errorf("live chat for repo %d: %w", repoID, ErrNotFound)
	}
	if err != nil {
		return Chat{}, fmt.Errorf("get live chat: %w", err)
	}
	return c, nil
}

// SaveChat writes the two mutable fields: the session and whether it has been
// archived.
func (s *Store) SaveChat(ctx context.Context, c Chat) error {
	c.UpdatedAt = time.Now().UTC()
	archived := ""
	if !c.ArchivedAt.IsZero() {
		archived = c.ArchivedAt.UTC().Format(rfc3339)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE chats SET session=?, archived_at=?, updated_at=? WHERE id=?`,
		c.Session, archived, c.UpdatedAt.Format(rfc3339), c.ID)
	if err != nil {
		return fmt.Errorf("update chat: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("chat %d: %w", c.ID, ErrNotFound)
	}
	return nil
}

// AddChatTurn records one thing said.
func (s *Store) AddChatTurn(ctx context.Context, t ChatTurn) (ChatTurn, error) {
	if t.ChatID == 0 {
		return ChatTurn{}, errors.New("a chat turn needs a chat")
	}
	if t.Speaker != SpeakerOperator && t.Speaker != SpeakerAssistant {
		return ChatTurn{}, fmt.Errorf("unknown chat speaker %q", t.Speaker)
	}
	t.CreatedAt = time.Now().UTC()

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO chat_turns (chat_id, speaker, body, cost_usd, input_tokens,
			output_tokens, provider, err_msg, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		t.ChatID, t.Speaker, t.Body, t.CostUSD, t.InputTokens, t.OutputTokens,
		t.Provider, t.ErrMsg, t.CreatedAt.Format(rfc3339))
	if err != nil {
		return ChatTurn{}, fmt.Errorf("insert chat turn: %w", err)
	}
	if t.ID, err = res.LastInsertId(); err != nil {
		return ChatTurn{}, fmt.Errorf("chat turn id: %w", err)
	}
	return t, nil
}

func scanChatTurn(sc interface{ Scan(...any) error }) (ChatTurn, error) {
	var t ChatTurn
	var created string
	err := sc.Scan(&t.ID, &t.ChatID, &t.Speaker, &t.Body, &t.CostUSD,
		&t.InputTokens, &t.OutputTokens, &t.Provider, &t.ErrMsg, &created)
	if err != nil {
		return ChatTurn{}, err
	}
	if t.CreatedAt, err = time.Parse(rfc3339, created); err != nil {
		return ChatTurn{}, fmt.Errorf("parse chat turn created_at: %w", err)
	}
	return t, nil
}

// ChatTurns returns a conversation oldest first.
//
// limit bounds it to the newest that many turns, still in reading order. Zero
// or less means all of it. The overlay always passes a limit: it re-renders on
// every state event, and a conversation that has been going for months is not
// something to put through SQLite several times a minute.
func (s *Store) ChatTurns(ctx context.Context, chatID int64, limit int) ([]ChatTurn, error) {
	query := `SELECT ` + chatTurnColumns + ` FROM chat_turns WHERE chat_id = ? ORDER BY id ASC`
	args := []any{chatID}
	if limit > 0 {
		// The newest N, then reversed by the caller's loop below, so the tail
		// still reads forwards.
		query = `SELECT * FROM (SELECT ` + chatTurnColumns + ` FROM chat_turns
			WHERE chat_id = ? ORDER BY id DESC LIMIT ?) ORDER BY id ASC`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query chat turns: %w", err)
	}
	defer rows.Close()

	var out []ChatTurn
	for rows.Next() {
		t, err := scanChatTurn(rows)
		if err != nil {
			return nil, fmt.Errorf("scan chat turn: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CountChatTurns is how long the conversation actually is, so a bounded render
// can say what it is not showing.
func (s *Store) CountChatTurns(ctx context.Context, chatID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chat_turns WHERE chat_id = ?`, chatID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count chat turns: %w", err)
	}
	return n, nil
}

// LastChatTurn returns the most recent turn, or ErrNotFound.
//
// Busy is derived from who spoke last, and that question is asked on every
// render. Loading a whole conversation to look at its final element is fine for
// a dozen design turns and not fine for a chat that has been alive for months.
func (s *Store) LastChatTurn(ctx context.Context, chatID int64) (ChatTurn, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+chatTurnColumns+`
		FROM chat_turns WHERE chat_id = ? ORDER BY id DESC LIMIT 1`, chatID)
	t, err := scanChatTurn(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatTurn{}, fmt.Errorf("chat %d has no turns: %w", chatID, ErrNotFound)
	}
	if err != nil {
		return ChatTurn{}, fmt.Errorf("get last chat turn: %w", err)
	}
	return t, nil
}

// ChatSpend is what a conversation has cost so far.
func (s *Store) ChatSpend(ctx context.Context, chatID int64) (float64, error) {
	var cost sql.NullFloat64
	err := s.db.QueryRowContext(ctx,
		`SELECT SUM(cost_usd) FROM chat_turns WHERE chat_id = ?`, chatID).Scan(&cost)
	if err != nil {
		return 0, fmt.Errorf("chat spend: %w", err)
	}
	return cost.Float64, nil
}

// ChatProposals returns every task list pulled out of a conversation, newest
// first.
//
// Deliberately unfiltered by state, unlike ListProposals. A pull that found
// nothing is discarded and a pull that was acted on is queued, and the
// conversation still has to be able to say what happened to its own last pull.
func (s *Store) ChatProposals(ctx context.Context, chatID int64) ([]Proposal, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+proposalColumns+` FROM proposals WHERE chat_id = ? ORDER BY id DESC`, chatID)
	if err != nil {
		return nil, fmt.Errorf("query chat proposals: %w", err)
	}
	defer rows.Close()

	var out []Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, fmt.Errorf("scan chat proposal: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ChatPulledGoals is what this conversation has already produced, across every
// pull, whatever became of it.
//
// Whether the operator queued a proposed task, left it, or discarded the whole
// pull, proposing it again is noise: they have seen it and decided. limit caps
// the list so a long conversation cannot grow the prompt without bound.
func (s *Store) ChatPulledGoals(ctx context.Context, chatID int64, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.goal FROM proposal_tasks t
		JOIN proposals p ON p.id = t.proposal_id
		WHERE p.chat_id = ?
		ORDER BY t.id DESC
		LIMIT ?`, chatID, limit)
	if err != nil {
		return nil, fmt.Errorf("query pulled goals: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var goal string
		if err := rows.Scan(&goal); err != nil {
			return nil, fmt.Errorf("scan pulled goal: %w", err)
		}
		if goal = strings.TrimSpace(goal); goal != "" {
			out = append(out, goal)
		}
	}
	return out, rows.Err()
}

// ChatPullInFlight reports whether this conversation already has a pull
// running.
//
// Derived state rather than a lock, the same way busy is: a lock in memory dies
// with the process, and a second pull started after a restart would spend money
// producing a list the first one was already producing.
func (s *Store) ChatPullInFlight(ctx context.Context, chatID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM proposals WHERE chat_id = ? AND state = ?`,
		chatID, ProposalAnalysing).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("count running pulls: %w", err)
	}
	return n > 0, nil
}

// FailStrandedChatTurns answers every live conversation left waiting on a reply
// by a daemon restart.
//
// A turn is a goroutine, not a claimable task, so nothing picks it back up.
// Busy is derived from the operator having spoken last, so left alone the
// overlay says "thinking…" for ever — the same lie FailStrandedProposals exists
// to prevent for an analysis. Archived chats are left alone: nothing is coming
// for them and nothing is waiting.
func (s *Store) FailStrandedChatTurns(ctx context.Context, reason string) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id FROM chats c
		WHERE c.archived_at = ''
		  AND (SELECT t.speaker FROM chat_turns t
		       WHERE t.chat_id = c.id ORDER BY t.id DESC LIMIT 1) = ?`,
		SpeakerOperator)
	if err != nil {
		return 0, fmt.Errorf("find stranded chats: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan stranded chat: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, id := range ids {
		_, err := s.AddChatTurn(ctx, ChatTurn{
			ChatID: id, Speaker: SpeakerAssistant,
			Body: "I could not reply: " + reason, ErrMsg: reason,
		})
		if err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}
