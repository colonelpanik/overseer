package web

import (
	"context"
	"fmt"
	"strings"
	"time"

	"overseer/internal/store"
)

// analysisHistoryLimit bounds the history list. Analyses accumulate slowly —
// one per repository per sitting — so fifty is a long memory, and the list is
// something to scan rather than page through.
const analysisHistoryLimit = 50

// buildAnalyses assembles the history list and the nav chip.
//
// The chip exists because the wizard lives in the URL: close that tab and the
// only link to a running analysis goes with it. The history exists because an
// analysis is worth more than the one sitting it was made in — queueing three
// of twelve and coming back for the rest is the normal way to use a long
// proposal.
func (s *Server) buildAnalyses(ctx context.Context, q Query) (*RunningAnalysis, []AnalysisRow, error) {
	history, err := s.store.AllProposals(ctx, analysisHistoryLimit)
	if err != nil {
		return nil, nil, err
	}

	var chip *RunningAnalysis
	rows := make([]AnalysisRow, 0, len(history))
	for _, h := range history {
		row := AnalysisRow{
			ID:        h.ID,
			State:     h.State,
			Tone:      analysisTone(h.State),
			Repo:      analysisSource(h.Proposal),
			When:      humanAge(h.CreatedAt),
			Spend:     money(h.CostUSD),
			Focus:     strings.Join(h.Focus, ", "),
			Remaining: h.Tasks - h.Queued,
			Err:       h.ErrMsg,
		}
		switch {
		case h.Tasks == 0:
			row.Progress = "no tasks proposed"
		default:
			row.Progress = fmt.Sprintf("%d of %d queued", h.Queued, h.Tasks)
		}
		// Discarded analyses stay on the list as a record of what was looked
		// at and what it cost, but there is nothing to reopen.
		if h.State != store.ProposalDiscarded {
			row.OpenURL = q.URL("wizard", h.ID, "overlay", "")
		}
		rows = append(rows, row)

		// The chip points at the most recent analysis that still wants
		// something: one running, or one with tasks left to queue.
		if chip == nil {
			switch h.State {
			case store.ProposalAnalysing, store.ProposalCloning:
				chip = &RunningAnalysis{
					Label: "analysing " + row.Repo,
					URL:   q.URL("wizard", h.ID, "overlay", ""),
					Live:  true,
				}
			case store.ProposalReady:
				chip = &RunningAnalysis{
					Label: fmt.Sprintf("%d task%s to review", row.Remaining, plural2(row.Remaining)),
					URL:   q.URL("wizard", h.ID, "overlay", ""),
				}
			}
		}
	}
	return chip, rows, nil
}

func analysisTone(state string) string {
	switch state {
	case store.ProposalFailed:
		return ToneAlert
	case store.ProposalAnalysing, store.ProposalCloning:
		return ToneLive
	default:
		return ToneMuted
	}
}

// analysisSource names what an analysis was pointed at, preferring the
// repository over the URL it was cloned from once the clone exists.
func analysisSource(p store.Proposal) string {
	if p.RepoPath != "" {
		return repoName(p.RepoPath)
	}
	if p.SourceURL != "" {
		return p.SourceURL
	}
	return "—"
}

// humanAge renders when something happened, coarsely. The history is scanned
// for "was that the one from this morning", not read for timestamps.
func humanAge(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2 Jan")
	}
}

func plural2(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
