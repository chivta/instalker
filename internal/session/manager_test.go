package session

import (
	"context"
	"errors"
	"testing"

	"github.com/arvlas/instalker/internal/domain"
)

const (
	validSession   = "10000000001:AaAaAaAaAaAaAa:25:FakeTokenValue"
	otherSession   = "20000000002:BbBbBbBbBbBbBb:9:OtherFakeToken"
	invalidSession = "not-a-session"
)

type fakeStore struct {
	stored  string
	loadErr error
	saveErr error
	saves   int
}

func (f *fakeStore) Session(context.Context) (string, error) {
	if f.loadErr != nil {
		return "", f.loadErr
	}
	if f.stored == "" {
		return "", domain.ErrNotFound
	}
	return f.stored, nil
}

func (f *fakeStore) SetSession(_ context.Context, sessionID string) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saves++
	f.stored = sessionID
	return nil
}

type fakeClient struct {
	applied string
}

func (f *fakeClient) SetSession(sessionID string) error {
	if sessionID == invalidSession {
		return domain.ErrUnauthorized
	}
	f.applied = sessionID
	return nil
}

func TestLoadPrefersStoredSession(t *testing.T) {
	store := &fakeStore{stored: validSession}
	client := &fakeClient{}

	err := New(store, client).Load(context.Background(), otherSession)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if client.applied != validSession {
		t.Errorf("applied %q, want the stored session", client.applied)
	}
	// The bootstrap must not overwrite a session that was rotated at runtime.
	if store.saves != 0 {
		t.Errorf("stored session was rewritten %d times, want 0", store.saves)
	}
}

func TestLoadSeedsFromBootstrap(t *testing.T) {
	store := &fakeStore{}
	client := &fakeClient{}

	err := New(store, client).Load(context.Background(), validSession)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if client.applied != validSession {
		t.Errorf("applied %q, want the bootstrap session", client.applied)
	}
	if store.stored != validSession {
		t.Errorf("stored %q, want the bootstrap session persisted", store.stored)
	}
}

func TestLoadWithoutAnySession(t *testing.T) {
	err := New(&fakeStore{}, &fakeClient{}).Load(context.Background(), "")

	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound so the caller can try a password login", err)
	}
}

func TestUpdateRejectsInvalidWithoutStoring(t *testing.T) {
	store := &fakeStore{stored: validSession}
	client := &fakeClient{}

	err := New(store, client).Update(context.Background(), invalidSession)
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("got %v, want ErrUnauthorized", err)
	}

	// A typo must not survive a restart.
	if store.stored != validSession {
		t.Errorf("store now holds %q, want the previous session untouched", store.stored)
	}
}

func TestUpdateAppliesAndPersists(t *testing.T) {
	store := &fakeStore{stored: validSession}
	client := &fakeClient{}

	err := New(store, client).Update(context.Background(), otherSession)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	if client.applied != otherSession {
		t.Errorf("applied %q, want the new session", client.applied)
	}
	if store.stored != otherSession {
		t.Errorf("stored %q, want the new session", store.stored)
	}
}
