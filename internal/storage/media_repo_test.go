package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/arvlas/instalker/internal/domain"
)

func TestMediaRepo(t *testing.T) {
	ctx := context.Background()

	db, err := Open(ctx, filepath.Join(t.TempDir(), "nested", "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	repo := NewMediaRepo(db)
	owner := domain.User{PK: "42", Username: "target"}
	media := domain.Media{ID: "m1", Kind: domain.KindPost, Owner: owner, TakenAt: time.Unix(1700000000, 0)}

	seen, err := repo.Seen(ctx, owner.PK, domain.KindPost, media.ID)
	if err != nil {
		t.Fatalf("seen: %v", err)
	}
	if seen {
		t.Fatal("fresh media reported as seen")
	}

	err = repo.MarkSeen(ctx, media)
	if err != nil {
		t.Fatalf("mark seen: %v", err)
	}

	// Marking twice must not fail on the primary key.
	err = repo.MarkSeen(ctx, media)
	if err != nil {
		t.Fatalf("re-mark seen: %v", err)
	}

	seen, err = repo.Seen(ctx, owner.PK, domain.KindPost, media.ID)
	if err != nil {
		t.Fatalf("seen after mark: %v", err)
	}
	if !seen {
		t.Fatal("marked media not reported as seen")
	}

	// The same id under the other kind is a different item.
	seen, err = repo.Seen(ctx, owner.PK, domain.KindStory, media.ID)
	if err != nil {
		t.Fatalf("seen story: %v", err)
	}
	if seen {
		t.Fatal("post id leaked into story namespace")
	}

	initialized, err := repo.Initialized(ctx, owner.PK)
	if err != nil {
		t.Fatalf("initialized: %v", err)
	}
	if initialized {
		t.Fatal("unknown target reported as initialized")
	}

	err = repo.MarkInitialized(ctx, owner)
	if err != nil {
		t.Fatalf("mark initialized: %v", err)
	}

	// Upsert path: marking again must not violate the primary key.
	err = repo.MarkInitialized(ctx, owner)
	if err != nil {
		t.Fatalf("re-mark initialized: %v", err)
	}

	initialized, err = repo.Initialized(ctx, owner.PK)
	if err != nil {
		t.Fatalf("initialized after mark: %v", err)
	}
	if !initialized {
		t.Fatal("target not reported as initialized")
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	for i := range 2 {
		db, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		db.Close()
	}
}

func TestStateRepoSession(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	repo := NewStateRepo(db)

	_, err = repo.Session(ctx)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound on an empty store", err)
	}

	err = repo.SetSession(ctx, "111:aaa:1")
	if err != nil {
		t.Fatalf("set session: %v", err)
	}

	got, err := repo.Session(ctx)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if got != "111:aaa:1" {
		t.Fatalf("session = %q, want the stored value", got)
	}

	// Replacing must overwrite rather than fail on the primary key.
	err = repo.SetSession(ctx, "222:bbb:2")
	if err != nil {
		t.Fatalf("replace session: %v", err)
	}

	got, err = repo.Session(ctx)
	if err != nil {
		t.Fatalf("session after replace: %v", err)
	}
	if got != "222:bbb:2" {
		t.Fatalf("session = %q, want the replacement", got)
	}

	// The session must survive a reopen, which is the entire point of storing it.
	db.Close()

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()

	got, err = NewStateRepo(db).Session(ctx)
	if err != nil {
		t.Fatalf("session after reopen: %v", err)
	}
	if got != "222:bbb:2" {
		t.Fatalf("session = %q after reopen, want it persisted", got)
	}
}

func TestTargetRepo(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "targets.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	repo := NewTargetRepo(db)

	got, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("list on an empty store: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d targets, want none", len(got))
	}

	err = repo.Save(ctx, []domain.User{
		{PK: "1", Username: "alpha"},
		{PK: "2", Username: "beta", IsPrivate: true},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err = repo.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got[0].Username != "alpha" || got[1].PK != "2" || !got[1].IsPrivate {
		t.Fatalf("round trip lost data: %+v", got)
	}

	// Saving again replaces rather than accumulating, so an account that is no
	// longer watched stops being polled.
	err = repo.Save(ctx, []domain.User{{PK: "3", Username: "gamma"}})
	if err != nil {
		t.Fatalf("re-save: %v", err)
	}

	got, err = repo.List(ctx)
	if err != nil {
		t.Fatalf("list after replace: %v", err)
	}
	if len(got) != 1 || got[0].Username != "gamma" {
		t.Fatalf("got %+v, want only gamma", got)
	}

	// The whole point is surviving a restart.
	db.Close()

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()

	got, err = NewTargetRepo(db).List(ctx)
	if err != nil {
		t.Fatalf("list after reopen: %v", err)
	}
	if len(got) != 1 || got[0].Username != "gamma" {
		t.Fatalf("targets did not survive a reopen: %+v", got)
	}
}

// An existing deployment already knows its accounts' ids from watch_state, so
// the cache starts populated rather than needing one more lookup to learn what
// the database has recorded for weeks.
func TestTargetCacheSeedsFromWatchState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "seed.db")

	// A database as it exists before the targets table was introduced.
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	_, err = db.ExecContext(ctx, `DELETE FROM targets`)
	if err != nil {
		t.Fatalf("clear targets: %v", err)
	}
	_, err = db.ExecContext(ctx, `DELETE FROM schema_version WHERE name = '003_targets.sql'`)
	if err != nil {
		t.Fatalf("rewind migration: %v", err)
	}

	err = NewMediaRepo(db).MarkInitialized(ctx, domain.User{PK: "6230019413", Username: "locroise"})
	if err != nil {
		t.Fatalf("seed watch state: %v", err)
	}
	db.Close()

	// Re-running the migration should carry those accounts across.
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()

	got, err := NewTargetRepo(db).List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].PK != "6230019413" || got[0].Username != "locroise" {
		t.Fatalf("cache was not seeded from watch_state: %+v", got)
	}
}
