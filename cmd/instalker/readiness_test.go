package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/arvlas/instalker/internal/domain"
	"github.com/arvlas/instalker/internal/poller"
	"github.com/arvlas/instalker/internal/schedule"
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

	watcher := poller.New(&stubInsta{}, nil, nil, nil, schedule.Always(time.Minute, time.Minute))
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

func TestCovers(t *testing.T) {
	cached := []domain.User{{Username: "locroise"}, {Username: "lem1rol"}}

	tests := []struct {
		name   string
		cached []domain.User
		wanted []string
		want   bool
	}{
		{"everything wanted is remembered", cached, []string{"locroise", "lem1rol"}, true},
		{"case does not matter", cached, []string{"LocRoise"}, true},
		{"a newly added account is missing", cached, []string{"locroise", "someone-new"}, false},
		{"nothing remembered", nil, []string{"locroise"}, false},
		// No names means the accounts come from the following list, so whatever
		// was remembered is what was being watched.
		{"no names requested", cached, nil, true},
		{"no names and nothing remembered", nil, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := covers(tt.cached, tt.wanted); got != tt.want {
				t.Errorf("covers(%v, %v) = %v, want %v", tt.cached, tt.wanted, got, tt.want)
			}
		})
	}
}
