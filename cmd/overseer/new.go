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
func cmdNew(cfg config.Config, path, brief string) error {
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

	ctx := context.Background()
	repo, err := eng.CreateProject(ctx, path)
	if err != nil {
		return err
	}
	p, err := eng.StartDesign(ctx, repo.Path, brief, true)
	if err != nil {
		return err
	}

	fmt.Printf("created %s and opened a design conversation\n", repo.Path)
	fmt.Printf("carry it on at http://%s/?wizard=%d\n", cfg.ListenAddr, p.ID)
	fmt.Println("(run `overseer serve` if the daemon is not already running)")
	return nil
}
