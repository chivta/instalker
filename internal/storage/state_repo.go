package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/arvlas/instalker/internal/domain"
)

// sessionKey is the app_state row holding the Instagram session cookie.
const sessionKey = "ig_session_id"

// StateRepo stores small pieces of mutable runtime state that must outlive a
// restart — currently just the Instagram session cookie, which rotates far more
// often than anything in the deployment manifest.
type StateRepo struct {
	db *sql.DB
}

// NewStateRepo builds a repository over an open database handle.
func NewStateRepo(db *sql.DB) *StateRepo {
	return &StateRepo{db: db}
}

// Session returns the stored session cookie, or domain.ErrNotFound when none
// has been stored yet.
func (r *StateRepo) Session(ctx context.Context) (string, error) {
	var value string

	err := r.db.QueryRowContext(ctx, `SELECT value FROM app_state WHERE key = ?`, sessionKey).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", domain.ErrNotFound
		}
		return "", fmt.Errorf("query session: %w", err)
	}

	return value, nil
}

// SetSession stores the session cookie, replacing any previous value.
func (r *StateRepo) SetSession(ctx context.Context, sessionID string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO app_state (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		sessionKey, sessionID, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}

	return nil
}
