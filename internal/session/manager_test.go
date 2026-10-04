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
	applied  string
	logins   int
	loginErr error
}

func (f *fakeClient) Login(context.Context, string, string) error {
	f.logins++
	if f.loginErr != nil {
		return f.loginErr
	}
	f.applied = "30000000003:FreshFromLogin:1:Token"
	return nil
}

func (f *fakeClient) SessionID() string {
	return f.applied
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

	err := New(store, client, "user", "pass").Load(context.Background(), otherSession)
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

	err := New(store, client, "user", "pass").Load(context.Background(), validSession)
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
	err := New(&fakeStore{}, &fakeClient{}, "user", "pass").Load(context.Background(), "")

	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound so the caller can try a password login", err)
	}
}

func TestUpdateRejectsInvalidWithoutStoring(t *testing.T) {
	store := &fakeStore{stored: validSession}
	client := &fakeClient{}

	err := New(store, client, "user", "pass").Update(context.Background(), invalidSession)
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

	err := New(store, client, "user", "pass").Update(context.Background(), otherSession)
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

func TestReloginStoresTheNewSession(t *testing.T) {
	store := &fakeStore{stored: validSession}
	client := &fakeClient{}

	err := New(store, client, "user", "pass").Relogin(context.Background())
	if err != nil {
		t.Fatalf("relogin: %v", err)
	}

	if store.stored != client.applied || client.logins != 1 {
		t.Errorf("stored %q after %d logins, want the login's session after one", store.stored, client.logins)
	}
}

// A burst of password logins is what earns a checkpoint, so a second attempt
// inside the cooldown must not reach Instagram.
func TestReloginCooldown(t *testing.T) {
	client := &fakeClient{loginErr: domain.ErrCheckpointRequired}
	manager := New(&fakeStore{}, client, "user", "pass")

	err := manager.Relogin(context.Background())
	if !errors.Is(err, domain.ErrCheckpointRequired) {
		t.Fatalf("first attempt: got %v, want the login error", err)
	}

	err = manager.Relogin(context.Background())
	if err == nil {
		t.Fatal("second attempt inside the cooldown succeeded")
	}
	if client.logins != 1 {
		t.Errorf("logged in %d times, want 1", client.logins)
	}
}

// A cookie put in IG_SESSIONID replaces a dead stored session without a
// password login, which matters when Instagram refuses logins from the host.
func TestReloginPrefersADifferentBootstrap(t *testing.T) {
	store := &fakeStore{stored: validSession}
	client := &fakeClient{}
	manager := New(store, client, "user", "pass")

	err := manager.Load(context.Background(), otherSession)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	err = manager.Relogin(context.Background())
	if err != nil {
		t.Fatalf("relogin: %v", err)
	}
	if client.applied != otherSession || store.stored != otherSession || client.logins != 0 {
		t.Fatalf("applied %q, stored %q after %d logins, want IG_SESSIONID without a login", client.applied, store.stored, client.logins)
	}

	// Tried once: the next failure goes to the password.
	err = manager.Relogin(context.Background())
	if err != nil {
		t.Fatalf("second relogin: %v", err)
	}
	if client.logins != 1 {
		t.Errorf("logins = %d, want the password tried after the bootstrap", client.logins)
	}
}

// When IG_SESSIONID is the session that just died, it is not worth retrying.
func TestReloginSkipsTheSameBootstrap(t *testing.T) {
	store := &fakeStore{stored: validSession}
	client := &fakeClient{}
	manager := New(store, client, "user", "pass")

	err := manager.Load(context.Background(), validSession)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	err = manager.Relogin(context.Background())
	if err != nil {
		t.Fatalf("relogin: %v", err)
	}
	if client.logins != 1 {
		t.Errorf("logins = %d, want a password login", client.logins)
	}
}
