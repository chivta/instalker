package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/arvlas/instalker/internal/domain"
)

type store interface {
	Session(ctx context.Context) (string, error)
	SetSession(ctx context.Context, sessionID string) error
}

type client interface {
	SetSession(sessionID string) error
	Login(ctx context.Context, username, password string) error
	SessionID() string
}

// reloginCooldown spaces out password logins. Instagram answers a burst of
// them with a checkpoint, which only a person can clear.
const reloginCooldown = 2 * time.Hour

// Manager owns the Instagram session cookie: where it is kept and how it is
// replaced.
//
// The stored cookie is the source of truth rather than the environment, because
// it expires on its own schedule and has to be replaceable without a redeploy.
type Manager struct {
	store    store
	client   client
	username string
	password string

	mu        sync.Mutex
	lastLogin time.Time
}

// New builds a manager over a persistent store, the client to apply the session
// to, and the credentials it logs back in with when Instagram ends a session.
func New(store store, client client, username, password string) *Manager {
	return &Manager{store: store, client: client, username: username, password: password}
}

// Relogin replaces a session Instagram has logged out with a fresh password
// login and stores it. Attempts are at most one per reloginCooldown; inside the
// cooldown it fails without contacting Instagram.
func (m *Manager) Relogin(ctx context.Context) error {
	m.mu.Lock()
	since := time.Since(m.lastLogin)
	if !m.lastLogin.IsZero() && since < reloginCooldown {
		m.mu.Unlock()
		return fmt.Errorf("%w: last login attempt was %s ago", domain.ErrRateLimited, since.Round(time.Minute))
	}
	m.lastLogin = time.Now()
	m.mu.Unlock()

	err := m.client.Login(ctx, m.username, m.password)
	if err != nil {
		return fmt.Errorf("password login: %w", err)
	}

	err = m.Store(ctx, m.client.SessionID())
	if err != nil {
		return err
	}
	log.Info().Msg("logged in again with the password, new session stored")

	return nil
}

// Load applies the stored session, falling back to bootstrap the first time
// there is nothing stored yet. It reports domain.ErrNotFound when neither is
// available, leaving the caller to decide what to do about it.
func (m *Manager) Load(ctx context.Context, bootstrap string) error {
	stored, err := m.store.Session(ctx)
	switch {
	case err == nil && stored != "":
		err = m.client.SetSession(stored)
		if err != nil {
			return fmt.Errorf("apply stored session: %w", err)
		}
		log.Info().Msg("using the stored instagram session")
		return nil
	case err != nil && !errors.Is(err, domain.ErrNotFound):
		return err
	}

	if bootstrap == "" {
		return domain.ErrNotFound
	}

	err = m.Update(ctx, bootstrap)
	if err != nil {
		return fmt.Errorf("seed session from IG_SESSIONID: %w", err)
	}
	log.Info().Msg("seeded the stored session from IG_SESSIONID")

	return nil
}

// Update validates a session, applies it to the live client and persists it.
//
// Applying comes first so an invalid value is rejected before it can be stored,
// which keeps a typo from surviving a restart.
func (m *Manager) Update(ctx context.Context, sessionID string) error {
	err := m.client.SetSession(sessionID)
	if err != nil {
		return err
	}

	return m.Store(ctx, sessionID)
}

// Store persists a session that has already been applied, used after a password
// login produces one.
func (m *Manager) Store(ctx context.Context, sessionID string) error {
	err := m.store.SetSession(ctx, sessionID)
	if err != nil {
		return err
	}

	return nil
}
