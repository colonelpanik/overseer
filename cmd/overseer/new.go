package main

import (
	"context"
	"fmt"
	"strings"

	"overseer/internal/config"
)

// cmdNew creates a project and opens the design conversation against it.
//
// The conversation itself belongs on the dashboard — it is a back-and-forth,
// and a terminal is a poor place to read an architect's reply and think about
// it. This gets you to the point where there is something to talk to, and says
// where to go.
//
// Which means waiting for the opening reply, not merely scheduling it. This
// process owns the store and closes it on the way out, so returning early both
// killed the turn and closed the database under whatever was left of it — and
// the URL it printed led to a conversation containing nothing but the brief.
//
// ctx is cancelled by SIGINT (see interruptible), which is the only way an
// operator can reach the agent: it runs in a process group of its own and never
// sees the terminal's Ctrl-C.
func cmdNew(ctx context.Context, cfg config.Config, path, brief string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("new needs a path for the project")
	}
	if strings.TrimSpace(brief) == "" {
		return fmt.Errorf("new needs -brief: say what you want built")
	}

	st, eng, err := open(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	repo, err := eng.CreateProject(ctx, path)
	if err != nil {
		return err
	}
	// Said before the wait rather than after it: the repository is already
	// there, and the reply takes as long as one agent turn takes.
	fmt.Printf("created %s\n", repo.Path)
	fmt.Println("opening a design conversation; the architect's first reply takes a minute or two")

	p, err := eng.StartDesignAndWait(ctx, repo.Path, brief, true)
	if err != nil {
		// The conversation exists whatever happened to the turn, and holds the
		// failure as a turn of its own, so say where to pick it up rather than
		// only that it broke. Interrupting this is the ordinary case: the
		// architect is stopped, the interruption is on the record, and the
		// dashboard can carry on from there.
		if p.ID != 0 {
			fmt.Printf("the conversation is at http://%s/?wizard=%d\n", cfg.ListenAddr, p.ID)
		}
		return fmt.Errorf("the architect could not open the conversation: %w", err)
	}

	fmt.Printf("carry it on at http://%s/?wizard=%d\n", cfg.ListenAddr, p.ID)
	fmt.Println("(run `overseer serve` if the daemon is not already running)")
	return nil
}
