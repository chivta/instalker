package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/arvlas/instalker/internal/domain"
	"github.com/arvlas/instalker/internal/poller"
)

func TestReadinessBeforeStartupFinishes(t *testing.T) {
	var ready readiness

	// Nothing has happened yet: /ping must still answer.
	_, err := ready.probe(context.Background())
	if !errors.Is(err, errStartingUp) {
		t.Fatalf("got %v, want errStartingUp", err)
	}

	// A throttled startup must surface as the cause, so the reply can say the
	// host is rate limited rather than asking for a new session.
	ready.stalled(fmt.Errorf("resolve targets: %w", domain.ErrRateLimited))

	_, err = ready.probe(context.Background())
	if !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("got %v, want the rate limit cause preserved", err)
	}
}

func TestReadinessOncePolling(t *testing.T) {
	var ready readiness
	ready.stalled(domain.ErrRateLimited)

	watcher := poller.New(&stubInsta{}, nil, nil, nil, time.Minute)
	ready.polling(watcher)

	probe, err := ready.probe(context.Background())
	if err != nil {
		t.Fatalf("probe after startup: %v", err)
	}
	// An empty target list is a real answer, not a startup failure.
	if len(probe.Targets) != 0 {
		t.Fatalf("got %d targets, want 0", len(probe.Targets))
	}
}

type stubInsta struct{}

func (stubInsta) Profile(context.Context, string) (domain.User, error) { return domain.User{}, nil }
func (stubInsta) Following(context.Context, string) ([]domain.User, error) {
	return nil, nil
}
func (stubInsta) Posts(context.Context, domain.User) ([]domain.Media, error) { return nil, nil }
func (stubInsta) Stories(context.Context, domain.User) ([]domain.Media, error) {
	return nil, nil
}
