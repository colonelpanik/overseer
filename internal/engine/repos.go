package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"overseer/internal/agent"
	"overseer/internal/config"
	"overseer/internal/store"
	"overseer/internal/worktree"
)

// EnsureRepo registers a repository, or returns the one already registered.
//
// Registration is a side effect of use rather than a step before it. Requiring
// an explicit `repo add` first would be a cleaner model and would break every
// task file that already exists, for nothing the upsert does not give: the
// first submit or analysis against a path registers it, and an operator who
// wants to add a repository without queueing anything calls exactly this.
func (e *Engine) EnsureRepo(ctx context.Context, repoPath string) (store.Repo, error) {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		return store.Repo{}, errors.New("no repository path given")
	}
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		return store.Repo{}, fmt.Errorf("resolve %s: %w", repoPath, err)
	}
	if err := checkRepo(ctx, abs); err != nil {
		return store.Repo{}, err
	}

	// A repository registered before its first commit has no default branch to
	// report, and one registered before `git remote add` has no origin. Both
	// errors are ignored on purpose — neither is a reason to refuse the
	// repository — but because UpsertRepo never overwrites a stored value with
	// a blank one, a value missed here would stay missing forever. So this runs
	// on every registration, and every submit calls it: the first commit or the
	// first remote is picked up on the next one.
	r := store.Repo{Path: abs, Detected: probeRepo(ctx, abs)}
	if branch, err := worktree.DefaultBranch(ctx, abs); err == nil {
		r.DefaultBranch = branch
	}
	if origin, err := worktree.OriginURL(ctx, abs); err == nil {
		r.OriginURL = origin
	}
	return e.Store.UpsertRepo(ctx, r)
}

// ResolveRepo turns a batch entry's `repo:` into a registered repository.
//
// A task file may name a repository by path, as it always has, or by the short
// slug the repo list shows — which is the whole point of registering one: the
// path is typed once.
func (e *Engine) ResolveRepo(ctx context.Context, ref string) (store.Repo, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return store.Repo{}, errors.New("no repository given")
	}
	// A slug never contains a separator, so this cannot swallow a path.
	if !strings.ContainsRune(ref, filepath.Separator) && !strings.HasPrefix(ref, ".") {
		if r, err := e.Store.RepoBySlug(ctx, ref); err == nil {
			return r, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return store.Repo{}, err
		}
	}
	return e.EnsureRepo(ctx, ref)
}

// ArchiveRepo puts a repository away, or brings it back.
//
// Archiving hides a repository from the pickers without deleting anything: its
// tasks, analyses and backlog stay exactly where they are, because the record
// of what was done to a repository outlives the operator's interest in it.
func (e *Engine) ArchiveRepo(ctx context.Context, repoID int64, archived bool) error {
	r, err := e.Store.GetRepo(ctx, repoID)
	if err != nil {
		return err
	}
	if archived {
		r.ArchivedAt = time.Now().UTC()
	} else {
		r.ArchivedAt = time.Time{}
	}
	if err := e.Store.SaveRepo(ctx, r); err != nil {
		return err
	}
	e.notify(0)
	return nil
}

// SetRepoDefaults writes the settings new tasks on this repository inherit.
//
// Empty and zero mean "fall through to the daemon default", never "off": a
// repository that wants no verify gate simply has none configured anywhere.
func (e *Engine) SetRepoDefaults(ctx context.Context, repoID int64, verify, severity string, capUSD float64) error {
	if capUSD < 0 {
		return fmt.Errorf("cost cap %.2f must not be negative", capUSD)
	}
	if severity != "" && !validSeverity(severity) {
		return fmt.Errorf("blocking_severity %q must be one of %v", severity, config.ValidSeverities)
	}
	r, err := e.Store.GetRepo(ctx, repoID)
	if err != nil {
		return err
	}
	r.VerifyCommand = strings.TrimSpace(verify)
	r.BlockingSeverity = severity
	r.CostCapUSD = capUSD
	if err := e.Store.SaveRepo(ctx, r); err != nil {
		return err
	}
	e.notify(0)
	return nil
}

// AddBacklogItem records something worth doing, by hand.
func (e *Engine) AddBacklogItem(ctx context.Context, repoID int64, title, detail, severity string) (store.BacklogItem, error) {
	if strings.TrimSpace(title) == "" {
		return store.BacklogItem{}, errors.New("a backlog item needs a title")
	}
	// A backlog item carries a FINDING's severity, not a task's blocking
	// threshold: "nit" is the commonest thing on the list, and "any" — a
	// threshold meaning "block on everything" — is not something an item is.
	if severity != "" && !agent.ValidSeverity(severity) {
		return store.BacklogItem{}, fmt.Errorf("severity %q must be one of %v", severity, agent.SeverityNames)
	}
	if _, err := e.Store.GetRepo(ctx, repoID); err != nil {
		return store.BacklogItem{}, err
	}
	item, err := e.Store.AddBacklogItem(ctx, store.BacklogItem{
		RepoID:   repoID,
		Source:   store.BacklogManual,
		Title:    strings.TrimSpace(title),
		Detail:   strings.TrimSpace(detail),
		Severity: severity,
	})
	if err != nil {
		return store.BacklogItem{}, err
	}
	e.notify(0)
	return item, nil
}

// DismissBacklogItem marks an item as not worth doing. It stays on the record
// and stops being offered — and stops being re-raised, since a repeat bumps the
// count without reopening it.
func (e *Engine) DismissBacklogItem(ctx context.Context, itemID int64) error {
	item, err := e.Store.GetBacklogItem(ctx, itemID)
	if err != nil {
		return err
	}
	if item.State == store.BacklogQueued {
		return fmt.Errorf("backlog item %d already became task %d", itemID, item.CreatedTaskID)
	}
	item.State = store.BacklogDismissed
	if err := e.Store.SaveBacklogItem(ctx, item); err != nil {
		return err
	}
	e.notify(0)
	return nil
}

// ReopenBacklogItem undoes a dismissal.
func (e *Engine) ReopenBacklogItem(ctx context.Context, itemID int64) error {
	item, err := e.Store.GetBacklogItem(ctx, itemID)
	if err != nil {
		return err
	}
	if item.State == store.BacklogQueued {
		return fmt.Errorf("backlog item %d already became task %d", itemID, item.CreatedTaskID)
	}
	item.State = store.BacklogOpen
	if err := e.Store.SaveBacklogItem(ctx, item); err != nil {
		return err
	}
	e.notify(0)
	return nil
}

// PromoteBacklogItem turns an item into a real task.
//
// The item's evidence is carried into the task's constraints rather than
// dropped: the citation is the most valuable thing a review finding has, and
// making the agent rediscover a file:line somebody already found is paying
// twice for the same work.
func (e *Engine) PromoteBacklogItem(ctx context.Context, itemID int64) (store.Task, error) {
	item, err := e.Store.GetBacklogItem(ctx, itemID)
	if err != nil {
		return store.Task{}, err
	}
	if item.State == store.BacklogQueued {
		return store.Task{}, fmt.Errorf("backlog item %d already became task %d", itemID, item.CreatedTaskID)
	}
	repo, err := e.Store.GetRepo(ctx, item.RepoID)
	if err != nil {
		return store.Task{}, err
	}

	constraints := backlogConstraints(item)
	task, err := e.Submit(ctx, BatchTask{
		Repo:        repo.Path,
		Goal:        item.Title,
		Constraints: constraints,
		// Severity and verify are left empty on purpose: Submit resolves them
		// task > repo > daemon, so the item inherits the repository's defaults
		// rather than carrying a copy that goes stale the moment they change.
	})
	if err != nil {
		return store.Task{}, err
	}

	item.State = store.BacklogQueued
	item.CreatedTaskID = task.ID
	if err := e.Store.SaveBacklogItem(ctx, item); err != nil {
		return task, err
	}
	e.notify(task.ID)
	return task, nil
}

// backlogConstraints turns an item's detail, evidence and provenance into the
// constraint lines a task starts from.
func backlogConstraints(item store.BacklogItem) []string {
	var out []string
	if d := strings.TrimSpace(item.Detail); d != "" {
		out = append(out, d)
	}
	if len(item.Evidence) > 0 {
		out = append(out, "Evidence: "+strings.Join(item.Evidence, ", "))
	}
	switch {
	case item.Source == store.BacklogReview && item.OriginTaskID != 0:
		out = append(out, fmt.Sprintf(
			"Raised by a review of task %d, below its blocking threshold, so the "+
				"loop did not act on it at the time.", item.OriginTaskID))
	case item.Source == store.BacklogAnalysis:
		out = append(out, "Proposed by a repository analysis.")
	}
	if item.Seen > 1 {
		out = append(out, fmt.Sprintf("Raised %d times.", item.Seen))
	}
	return out
}

// recordBacklogFindings files a review's non-blocking findings on the
// repository's backlog.
//
// These are the findings the loop deliberately did not act on. Before the
// backlog they were displayed on the finding ledger and could never become
// anything: the reviewer's judgement about a real problem was shown once and
// then thrown away. Blocking findings are not copied, because the loop is
// already acting on those and a list that mostly duplicates work in flight is
// not a working list.
//
// It returns an error for the caller to decide about; recordFindings drops it,
// because the task's own outcome does not depend on its backlog and losing a
// nit is a far smaller thing than failing a turn that otherwise succeeded.
func (e *Engine) recordBacklogFindings(ctx context.Context, task *store.Task, findings []store.Finding) error {
	if task.RepoID == 0 {
		return nil
	}
	for _, f := range findings {
		if f.Blocking || strings.TrimSpace(f.Summary) == "" {
			continue
		}
		item := store.BacklogItem{
			RepoID:       task.RepoID,
			Source:       store.BacklogReview,
			Title:        f.Summary,
			Severity:     f.Severity,
			OriginTaskID: task.ID,
			FindingID:    f.ID,
		}
		if f.File != "" {
			cite := f.File
			if f.Line > 0 {
				cite = fmt.Sprintf("%s:%d", f.File, f.Line)
			}
			item.Evidence = []string{cite}
		}
		if _, err := e.Store.AddBacklogItem(ctx, item); err != nil {
			return fmt.Errorf("file finding from task %d on the backlog: %w", task.ID, err)
		}
	}
	return nil
}

// recordBacklogProposals files an analysis's unqueued tasks on the backlog.
//
// A proposal is a record of one run; the backlog is the working list. Queueing
// three of twelve used to mean the other nine existed only inside that
// proposal, findable if you remembered which analysis it was. Now they land
// somewhere durable, deduplicated against everything else already known about
// the repository.
func (e *Engine) recordBacklogProposals(ctx context.Context, p store.Proposal, rows []store.ProposalTask) error {
	if p.RepoID == 0 {
		return nil
	}
	for _, r := range rows {
		if r.CreatedTaskID != 0 || strings.TrimSpace(r.Goal) == "" {
			continue
		}
		item := store.BacklogItem{
			RepoID:         p.RepoID,
			Source:         store.BacklogAnalysis,
			Title:          r.Goal,
			Detail:         r.Rationale,
			Evidence:       r.Evidence,
			Severity:       r.Severity,
			ProposalTaskID: r.ID,
		}
		if _, err := e.Store.AddBacklogItem(ctx, item); err != nil {
			return fmt.Errorf("file proposal %d on the backlog: %w", p.ID, err)
		}
	}
	return nil
}

// RepoStats is the per-repository accounting, with the metered/reported split
// taken from the live provider table.
func (e *Engine) RepoStats(ctx context.Context) (map[int64]store.RepoStats, error) {
	return e.Store.RepoStats(ctx, e.roleConfig().MeteredProviders())
}

// RepoSpend is the same split, totalled, for the nav.
func (e *Engine) RepoSpend(ctx context.Context) (reported, metered float64, err error) {
	return e.Store.RepoSpend(ctx, e.roleConfig().MeteredProviders())
}

func validSeverity(s string) bool {
	for _, v := range config.ValidSeverities {
		if s == v {
			return true
		}
	}
	return false
}
