package notifier

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/arvlas/instalker/internal/domain"
)

func TestFormatProbe(t *testing.T) {
	target := domain.User{Username: "locroise"}
	throttled := fmt.Errorf("posts: %w", domain.ErrRateLimited)

	tests := []struct {
		name        string
		probe       domain.Probe
		wantContain []string
		wantAbsent  []string
	}{
		{
			name: "healthy target",
			probe: domain.Probe{
				Elapsed: 1200 * time.Millisecond,
				Targets: []domain.TargetProbe{{
					User:    target,
					Posts:   domain.FeedProbe{Count: 12, Latest: time.Now().Add(-2 * time.Hour)},
					Stories: domain.FeedProbe{Count: 2},
				}},
			},
			wantContain: []string{"working", "locroise", "12 posts", "2 stories", "1.2s"},
			wantAbsent:  []string{"failing", "partly"},
		},
		{
			// The state that used to be reported as an outright failure, hiding
			// that stories were being served the whole time.
			name: "posts throttled while stories work",
			probe: domain.Probe{
				Targets: []domain.TargetProbe{{
					User:    target,
					Posts:   domain.FeedProbe{Err: throttled},
					Stories: domain.FeedProbe{Count: 3},
				}},
			},
			wantContain: []string{"partly working", "posts: rate limited", "3 stories"},
			wantAbsent:  []string{"is failing"},
		},
		{
			name: "both feeds throttled",
			probe: domain.Probe{
				Targets: []domain.TargetProbe{{
					User:    target,
					Posts:   domain.FeedProbe{Err: throttled},
					Stories: domain.FeedProbe{Err: throttled},
				}},
			},
			wantContain: []string{"is failing", "rate limited"},
			// Rotating the cookie does not clear a 429, so do not suggest it.
			wantAbsent: []string{"/session"},
		},
		{
			name: "rejected session",
			probe: domain.Probe{
				Targets: []domain.TargetProbe{{
					User:    target,
					Posts:   domain.FeedProbe{Err: fmt.Errorf("posts: %w", domain.ErrUnauthorized)},
					Stories: domain.FeedProbe{Err: fmt.Errorf("stories: %w", domain.ErrUnauthorized)},
				}},
			},
			wantContain: []string{"is failing", "/session"},
		},
		{
			name:        "no targets",
			probe:       domain.Probe{},
			wantContain: []string{"No targets"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatProbe(tt.probe)

			for _, want := range tt.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in:\n%s", want, got)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("unexpected %q in:\n%s", absent, got)
				}
			}
		})
	}
}

// A partly working check must not read as healthy, or a throttled feed goes
// unnoticed for as long as the other one keeps answering.
func TestPartialProbeIsNotOK(t *testing.T) {
	probe := domain.Probe{
		Targets: []domain.TargetProbe{{
			Posts:   domain.FeedProbe{Err: domain.ErrRateLimited},
			Stories: domain.FeedProbe{Count: 1},
		}},
	}

	if probe.OK() {
		t.Error("a probe with a throttled feed reported OK")
	}
	if probe.Failed() {
		t.Error("a probe with a working feed reported total failure")
	}
}

// A username is attacker-controlled text going into an HTML-parsed message.
func TestFormatProbeEscapesUsername(t *testing.T) {
	probe := domain.Probe{
		Targets: []domain.TargetProbe{{
			User:  domain.User{Username: "<b>evil</b>"},
			Posts: domain.FeedProbe{Count: 1},
		}},
	}

	got := formatProbe(probe)
	if strings.Contains(got, "<b>evil</b>") {
		t.Fatalf("username was not escaped:\n%s", got)
	}
	if !strings.Contains(got, "&lt;b&gt;evil&lt;/b&gt;") {
		t.Fatalf("expected escaped username:\n%s", got)
	}
}
