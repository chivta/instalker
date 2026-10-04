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
			// What Instagram tells an anonymous visitor. The "wait a few
			// minutes" text reads like throttling and hid a dead session for a
			// month; require_login is the part that matters.
			name:   "logged out despite the wait message",
			status: http.StatusUnauthorized,
			body:   `{"message":"Please wait a few minutes before you try again.","require_login":true,"igweb_rollout":true,"status":"fail"}`,
			want:   domain.ErrUnauthorized,
		},
		{
			name:   "wait message without require_login",
			status: http.StatusBadRequest,
			body:   `{"message":"Please wait a few minutes before you try again.","status":"fail"}`,
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

func TestParseTokens(t *testing.T) {
	page := `<html><script id="__eqmc" type="application/json">{"e":"1","f":"DTSG-TOKEN","l":null}</script>` +
		`["LSD",[],{"token":"LSD-TOKEN"}]` +
		`<link href="https://static.cdninstagram.com/rsrc.php/v4/a.js" as="script">` +
		`<link href="https://static.cdninstagram.com/rsrc.php/v4/a.js" as="script">` +
		`<link href="https://static.cdninstagram.com/rsrc.php/v4/b.css" as="style">`

	tokens, err := parseTokens(page)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if tokens.dtsg != "DTSG-TOKEN" || tokens.lsd != "LSD-TOKEN" {
		t.Errorf("tokens = %+v", tokens)
	}
	if len(tokens.scripts) != 1 {
		t.Errorf("scripts = %v, want the one unique .js bundle", tokens.scripts)
	}

	// A page served to a logged-out visitor carries no fb_dtsg.
	_, err = parseTokens(`<html>["LSD",[],{"token":"x"}]</html>`)
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("logged-out page: got %v, want ErrUnauthorized", err)
	}
}
