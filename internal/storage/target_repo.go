package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/arvlas/instalker/internal/domain"
)

// TargetRepo remembers which accounts were resolved, so a restart does not have
// to ask Instagram again.
//
// Resolving a username costs a request, and those requests are on the critical
// path: a throttled lookup used to stop the bot from starting at all, even
// though the answer had not changed in weeks.
type TargetRepo struct {
	db *sql.DB
}

// NewTargetRepo builds a repository over an open database handle.
func NewTargetRepo(db *sql.DB) *TargetRepo {
	return &TargetRepo{db: db}
}

// List returns the remembered targets.
func (r *TargetRepo) List(ctx context.Context) ([]domain.User, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT pk, username, is_private FROM targets ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("query targets: %w", err)
	}
	defer rows.Close()

	var targets []domain.User
	for rows.Next() {
		var (
			user    domain.User
			private int
		)

		err = rows.Scan(&user.PK, &user.Username, &private)
		if err != nil {
			return nil, fmt.Errorf("scan target: %w", err)
		}
		user.IsPrivate = private == 1

		targets = append(targets, user)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("read targets: %w", err)
	}

	return targets, nil
}

// Save replaces the remembered targets with the given set.
func (r *TargetRepo) Save(ctx context.Context, targets []domain.User) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	// Replacing wholesale keeps an account that is no longer watched from
	// lingering and being polled forever.
	_, err = tx.ExecContext(ctx, `DELETE FROM targets`)
	if err != nil {
		return fmt.Errorf("clear targets: %w", err)
	}

	now := time.Now().Unix()
	for _, target := range targets {
		private := 0
		if target.IsPrivate {
			private = 1
		}

		_, err = tx.ExecContext(ctx,
			`INSERT INTO targets (pk, username, is_private, resolved_at) VALUES (?, ?, ?, ?)`,
			target.PK, target.Username, private, now,
		)
		if err != nil {
			return fmt.Errorf("insert target %s: %w", target.Username, err)
		}
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("commit targets: %w", err)
	}

	return nil
}
