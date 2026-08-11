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
				Targets: []domain.TargetProbe{
					{User: target, Posts: 12, Stories: 2, Latest: time.Now().Add(-2 * time.Hour)},
				},
			},
			wantContain: []string{"working", "locroise", "12 posts", "2 stories", "1.2s"},
			wantAbsent:  []string{"failing"},
		},
		{
			// A throttled host must not be reported as a session problem: the
			// fix is a different network, not a new cookie.
			name: "throttled target",
			probe: domain.Probe{
				Targets: []domain.TargetProbe{
					{User: target, Err: fmt.Errorf("posts: %w", domain.ErrRateLimited)},
				},
			},
			wantContain: []string{"failing", "rate limited"},
			wantAbsent:  []string{"IG_SESSIONID"},
		},
		{
			name: "rejected session",
			probe: domain.Probe{
				Targets: []domain.TargetProbe{
					{User: target, Err: fmt.Errorf("posts: %w", domain.ErrUnauthorized)},
				},
			},
			wantContain: []string{"failing", "IG_SESSIONID"},
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

// A username is attacker-controlled text going into an HTML-parsed message.
func TestFormatProbeEscapesUsername(t *testing.T) {
	probe := domain.Probe{
		Targets: []domain.TargetProbe{
			{User: domain.User{Username: "<b>evil</b>"}, Posts: 1},
		},
	}

	got := formatProbe(probe)
	if strings.Contains(got, "<b>evil</b>") {
		t.Fatalf("username was not escaped:\n%s", got)
	}
	if !strings.Contains(got, "&lt;b&gt;evil&lt;/b&gt;") {
		t.Fatalf("expected escaped username:\n%s", got)
	}
}
