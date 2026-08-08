package store

import (
	"context"
	"fmt"
)

// SetTaskDeps replaces the set of tasks that taskID waits for.
//
// It rejects a self-reference and any edge that would close a cycle, so the
// scheduler never has to reason about a group of tasks that can only wait for
// each other. Passing an empty list clears the dependencies, which is what the
// dashboard's "release anyway" does.
func (s *Store) SetTaskDeps(ctx context.Context, taskID int64, depIDs []int64) error {
	seen := make(map[int64]bool, len(depIDs))
	unique := make([]int64, 0, len(depIDs))
	for _, id := range depIDs {
		if id == taskID {
			return fmt.Errorf("task %d cannot depend on itself", taskID)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}

	graph, err := s.AllTaskDeps(ctx)
	if err != nil {
		return err
	}
	// Evaluate the cycle check against the graph as it would be after the
	// write, not as it is now: replacing an edge can break a cycle as easily
	// as create one.
	graph[taskID] = unique
	for _, id := range unique {
		if reaches(graph, id, taskID) {
			return fmt.Errorf("task %d depending on %d would create a cycle", taskID, id)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin deps tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM task_deps WHERE task_id = ?`, taskID); err != nil {
		return fmt.Errorf("clear deps: %w", err)
	}
	for _, id := range unique {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO task_deps (task_id, depends_on_id) VALUES (?, ?)`, taskID, id)
		if err != nil {
			return fmt.Errorf("insert dep %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit deps: %w", err)
	}
	return nil
}

// TaskDeps returns the IDs taskID waits for, in insertion order.
func (s *Store) TaskDeps(ctx context.Context, taskID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT depends_on_id FROM task_deps WHERE task_id = ? ORDER BY depends_on_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("query deps: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan dep: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AllTaskDeps returns the whole dependency graph in one query. The board needs
// every task's dependencies at once, and asking per task would be one
// statement per card.
func (s *Store) AllTaskDeps(ctx context.Context) (map[int64][]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT task_id, depends_on_id FROM task_deps ORDER BY task_id, depends_on_id`)
	if err != nil {
		return nil, fmt.Errorf("query dep graph: %w", err)
	}
	defer rows.Close()

	out := map[int64][]int64{}
	for rows.Next() {
		var task, dep int64
		if err := rows.Scan(&task, &dep); err != nil {
			return nil, fmt.Errorf("scan dep edge: %w", err)
		}
		out[task] = append(out[task], dep)
	}
	return out, rows.Err()
}

// reaches reports whether target is reachable from start by following
// dependency edges.
func reaches(graph map[int64][]int64, start, target int64) bool {
	stack := []int64{start}
	seen := map[int64]bool{}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == target {
			return true
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		stack = append(stack, graph[id]...)
	}
	return false
}
