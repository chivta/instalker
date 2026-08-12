package instagram

import (
	"errors"
	"net/http"
	"testing"

	"github.com/arvlas/instalker/internal/domain"
)

func TestStatusError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{
			name:   "ok",
			status: http.StatusOK,
			body:   `{"items":[]}`,
			want:   nil,
		},
		{
			// Instagram's throttle response: a 401 that says "wait a few
			// minutes". Reading it as a dead session provokes a password login
			// and a challenge, so the message has to win over the status.
			name:   "throttle disguised as 401",
			status: http.StatusUnauthorized,
			body:   `{"message":"Please wait a few minutes before you try again.","require_login":true}`,
			want:   domain.ErrRateLimited,
		},
		{
			name:   "throttle disguised as 200",
			status: http.StatusOK,
			body:   `{"message":"Please wait a few minutes before you try again.","require_login":true}`,
			want:   domain.ErrRateLimited,
		},
		{
			name:   "genuine rejection",
			status: http.StatusUnauthorized,
			body:   `{"message":"login_required"}`,
			want:   domain.ErrUnauthorized,
		},
		{
			name:   "checkpoint",
			status: http.StatusOK,
			body:   `{"message":"checkpoint_required"}`,
			want:   domain.ErrCheckpointRequired,
		},
		{
			name:   "explicit rate limit",
			status: http.StatusTooManyRequests,
			body:   ``,
			want:   domain.ErrRateLimited,
		},
		{
			name:   "not found",
			status: http.StatusNotFound,
			body:   ``,
			want:   domain.ErrNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := statusError(tt.status, []byte(tt.body))
			if tt.want == nil {
				if err != nil {
					t.Fatalf("got %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}

func TestSessionUser(t *testing.T) {
	tests := []struct {
		name    string
		session string
		wantPK  string
		wantErr bool
	}{
		{
			name:    "percent encoded cookie as copied from a browser",
			session: "10000000001%3AAaAaAaAaAaAaAa%3A25%3AFakeTokenValue",
			wantPK:  "10000000001",
		},
		{
			name:    "plain cookie",
			session: "10000000001:AaAaAaAaAaAaAa:25:FakeTokenValue",
			wantPK:  "10000000001",
		},
		{
			name:    "empty session",
			session: "",
			wantErr: true,
		},
		{
			name:    "no separator",
			session: "garbage",
			wantErr: true,
		},
		{
			name:    "non numeric account id",
			session: "notapk:token:25",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(tt.session)
			if err != nil {
				t.Fatalf("new client: %v", err)
			}

			me, err := c.SessionUser()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("got pk %q, want an error", me.PK)
				}
				return
			}
			if err != nil {
				t.Fatalf("session user: %v", err)
			}
			if me.PK != tt.wantPK {
				t.Errorf("pk = %q, want %q", me.PK, tt.wantPK)
			}
		})
	}
}
