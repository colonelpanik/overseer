package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Settings keys.
const (
	// SettingStopAll is the operator's global stop. Non-empty is the reason it
	// was stopped.
	//
	// Persisted, unlike the authentication pause, because the two mean
	// different things. An auth failure is a condition that may have cleared,
	// and a restart is exactly when it should be retried; an operator stopping
	// everything is a decision, and a daemon that quietly resumed on restart
	// would be overruling it.
	SettingStopAll = "stop_all"
)

// Setting reads one daemon-level setting. A key that was never written reads
// as empty rather than as an error, so a fresh database needs no seeding.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read setting %q: %w", key, err)
	}
	return value, nil
}

// SetSetting writes one daemon-level setting. An empty value deletes the row,
// so "unset" and "set to nothing" cannot drift apart.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if value == "" {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
			return fmt.Errorf("clear setting %q: %w", key, err)
		}
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?,?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UTC().Format(rfc3339))
	if err != nil {
		return fmt.Errorf("write setting %q: %w", key, err)
	}
	return nil
}
