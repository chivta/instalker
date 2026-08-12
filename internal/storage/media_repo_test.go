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
