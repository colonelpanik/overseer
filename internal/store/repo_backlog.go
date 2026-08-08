package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Where a backlog item came from.
const (
	// BacklogAnalysis is a task an analysis proposed that nobody queued.
	BacklogAnalysis = "analysis"
	// BacklogReview is a review finding below the blocking threshold — the
	// loop deliberately did not act on it, and before the backlog existed it
	// had nowhere to go.
	BacklogReview = "review"
	// BacklogManual is something the operator wrote down.
	BacklogManual = "manual"
)

// Backlog item states.
const (
	BacklogOpen      = "open"
	BacklogQueued    = "queued"
	BacklogDismissed = "dismissed"
)

// BacklogItem is one thing worth doing to a repository that nothing is doing.
type BacklogItem struct {
	ID       int64
	RepoID   int64
	Source   string
	Title    string
	Detail   string
	Evidence []string
	Severity string
	// Fingerprint collapses repeats. Set by Add from the title when empty.
	Fingerprint string
	// Seen counts how many times this item has been raised. A nit the
	// reviewer produces on three separate tasks is one item seen three times,
	// which says far more than three identical rows would.
	Seen int

	ProposalTaskID int64
	FindingID      int64
	OriginTaskID   int64

	State         string
	CreatedTaskID int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

const backlogColumns = `id, repo_id, source, title, detail, evidence, severity,
	fingerprint, seen, proposal_task_id, finding_id, origin_task_id, state,
	created_task_id, created_at, updated_at`

// volatile matches the parts of a finding that change between otherwise
// identical occurrences: line numbers, durations, hex addresses. They are
// stripped before fingerprinting so the same complaint about a file that has
// since shifted by a few lines still collapses onto one item.
var volatile = regexp.MustCompile(`\b(0x[0-9a-f]+|\d+(\.\d+)?(ms|s|m|h)?)\b`)

// Fingerprint reduces a title to what makes it the same complaint twice.
func Fingerprint(title string) string {
	norm := volatile.ReplaceAllString(strings.ToLower(strings.TrimSpace(title)), "#")
	norm = strings.Join(strings.Fields(norm), " ")
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:16])
}

func scanBacklogItem(sc interface{ Scan(...any) error }) (BacklogItem, error) {
	var b BacklogItem
	var evidence, created, updated string
	err := sc.Scan(&b.ID, &b.RepoID, &b.Source, &b.Title, &b.Detail, &evidence,
		&b.Severity, &b.Fingerprint, &b.Seen, &b.ProposalTaskID, &b.FindingID,
		&b.OriginTaskID, &b.State, &b.CreatedTaskID, &created, &updated)
	if err != nil {
		return BacklogItem{}, err
	}
	b.Evidence = splitLines(evidence)
	if b.CreatedAt, err = time.Parse(rfc3339, created); err != nil {
		return BacklogItem{}, fmt.Errorf("parse backlog created_at: %w", err)
	}
	if b.UpdatedAt, err = time.Parse(rfc3339, updated); err != nil {
		return BacklogItem{}, fmt.Errorf("parse backlog updated_at: %w", err)
	}
	return b, nil
}

// AddBacklogItem records something worth doing, or bumps the count on an item
// already recorded.
//
// A repeat never creates a second row and never resurrects a dismissed one:
// an operator who said "not this" should not have to say it again every time
// the reviewer notices the same thing. It does still count, so a dismissed
// item that keeps coming back is visible as such.
func (s *Store) AddBacklogItem(ctx context.Context, b BacklogItem) (BacklogItem, error) {
	if b.RepoID == 0 {
		return BacklogItem{}, errors.New("a backlog item needs a repository")
	}
	if strings.TrimSpace(b.Title) == "" {
		return BacklogItem{}, errors.New("a backlog item needs a title")
	}
	if b.Fingerprint == "" {
		b.Fingerprint = Fingerprint(b.Title)
	}
	if b.State == "" {
		b.State = BacklogOpen
	}

	existing, err := s.backlogByFingerprint(ctx, b.RepoID, b.Fingerprint)
	switch {
	case err == nil:
		_, err := s.db.ExecContext(ctx, `
			UPDATE backlog SET seen = seen + 1, updated_at = ? WHERE id = ?`,
			time.Now().UTC().Format(rfc3339), existing.ID)
		if err != nil {
			return BacklogItem{}, fmt.Errorf("bump backlog item: %w", err)
		}
		existing.Seen++
		return existing, nil
	case !errors.Is(err, ErrNotFound):
		return BacklogItem{}, err
	}

	now := time.Now().UTC()
	b.CreatedAt, b.UpdatedAt, b.Seen = now, now, 1
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO backlog (repo_id, source, title, detail, evidence, severity,
			fingerprint, seen, proposal_task_id, finding_id, origin_task_id,
			state, created_task_id, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		b.RepoID, b.Source, b.Title, b.Detail, strings.Join(b.Evidence, "\n"),
		b.Severity, b.Fingerprint, b.Seen, b.ProposalTaskID, b.FindingID,
		b.OriginTaskID, b.State, b.CreatedTaskID,
		now.Format(rfc3339), now.Format(rfc3339))
	if err != nil {
		return BacklogItem{}, fmt.Errorf("insert backlog item: %w", err)
	}
	if b.ID, err = res.LastInsertId(); err != nil {
		return BacklogItem{}, fmt.Errorf("backlog item id: %w", err)
	}
	return b, nil
}

func (s *Store) backlogByFingerprint(ctx context.Context, repoID int64, fp string) (BacklogItem, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+backlogColumns+` FROM backlog WHERE repo_id = ? AND fingerprint = ?`,
		repoID, fp)
	b, err := scanBacklogItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return BacklogItem{}, ErrNotFound
	}
	if err != nil {
		return BacklogItem{}, fmt.Errorf("get backlog item: %w", err)
	}
	return b, nil
}

// GetBacklogItem loads one item by ID.
func (s *Store) GetBacklogItem(ctx context.Context, id int64) (BacklogItem, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+backlogColumns+` FROM backlog WHERE id = ?`, id)
	b, err := scanBacklogItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return BacklogItem{}, fmt.Errorf("backlog item %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return BacklogItem{}, fmt.Errorf("get backlog item: %w", err)
	}
	return b, nil
}

// SaveBacklogItem writes an item's mutable fields.
func (s *Store) SaveBacklogItem(ctx context.Context, b BacklogItem) error {
	b.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE backlog SET title=?, detail=?, evidence=?, severity=?,
			state=?, created_task_id=?, updated_at=?
		WHERE id=?`,
		b.Title, b.Detail, strings.Join(b.Evidence, "\n"), b.Severity,
		b.State, b.CreatedTaskID, b.UpdatedAt.Format(rfc3339), b.ID)
	if err != nil {
		return fmt.Errorf("update backlog item: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("backlog item %d: %w", b.ID, ErrNotFound)
	}
	return nil
}

// ListBacklog returns a repository's items, open ones first and newest within
// that, so the working list leads with what is actually actionable.
func (s *Store) ListBacklog(ctx context.Context, repoID int64) ([]BacklogItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+backlogColumns+`
		FROM backlog WHERE repo_id = ?
		ORDER BY CASE state WHEN 'open' THEN 0 WHEN 'queued' THEN 1 ELSE 2 END,
			seen DESC, id DESC`, repoID)
	if err != nil {
		return nil, fmt.Errorf("query backlog: %w", err)
	}
	defer rows.Close()

	var out []BacklogItem
	for rows.Next() {
		b, err := scanBacklogItem(rows)
		if err != nil {
			return nil, fmt.Errorf("scan backlog item: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// OpenBacklogCounts is how many open items each repository has, for the repo
// list. One query rather than one per repository.
func (s *Store) OpenBacklogCounts(ctx context.Context) (map[int64]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT repo_id, COUNT(*) FROM backlog WHERE state = ? GROUP BY repo_id`,
		BacklogOpen)
	if err != nil {
		return nil, fmt.Errorf("count backlog: %w", err)
	}
	defer rows.Close()

	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("scan backlog count: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}
