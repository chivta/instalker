package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/arvlas/instalker/internal/domain"
)

// MediaRepo records which media items have already been delivered to Telegram.
type MediaRepo struct {
	db *sql.DB
}

// NewMediaRepo builds a repository over an open database handle.
func NewMediaRepo(db *sql.DB) *MediaRepo {
	return &MediaRepo{db: db}
}

// Seen reports whether a media item has already been delivered.
func (r *MediaRepo) Seen(ctx context.Context, ownerPK string, kind domain.Kind, mediaID string) (bool, error) {
	var count int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM seen_media WHERE owner_pk = ? AND kind = ? AND media_id = ?`,
		ownerPK, string(kind), mediaID,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("query seen media: %w", err)
	}

	return count > 0, nil
}

// MarkSeen records a media item as delivered. Re-marking an item is a no-op.
func (r *MediaRepo) MarkSeen(ctx context.Context, media domain.Media) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO seen_media (owner_pk, kind, media_id, taken_at, seen_at) VALUES (?, ?, ?, ?, ?)`,
		media.Owner.PK, string(media.Kind), media.ID, media.TakenAt.Unix(), time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("insert seen media: %w", err)
	}

	return nil
}

// Initialized reports whether the first, baseline poll for a target has run.
// Until it has, existing media is recorded silently instead of being sent.
func (r *MediaRepo) Initialized(ctx context.Context, ownerPK string) (bool, error) {
	var initialized int
	err := r.db.QueryRowContext(ctx, `SELECT initialized FROM watch_state WHERE owner_pk = ?`, ownerPK).Scan(&initialized)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("query watch state: %w", err)
	}

	return initialized == 1, nil
}

// MarkInitialized flags a target as baselined.
func (r *MediaRepo) MarkInitialized(ctx context.Context, user domain.User) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO watch_state (owner_pk, username, initialized) VALUES (?, ?, 1)
		 ON CONFLICT(owner_pk) DO UPDATE SET username = excluded.username, initialized = 1`,
		user.PK, user.Username,
	)
	if err != nil {
		return fmt.Errorf("upsert watch state: %w", err)
	}

	return nil
}
