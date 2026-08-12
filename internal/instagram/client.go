package instagram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/arvlas/instalker/internal/domain"
)

const (
	baseURL   = "https://www.instagram.com"
	appID     = "936619743392459"
	userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

	requestTimeout = 30 * time.Second
	// maxBodySize caps how much of a response body is read before giving up.
	maxBodySize = 8 << 20
)

// Client talks to Instagram's web private API using a logged-in session cookie.
//
// The session can be replaced while the client is running (see SetSession), so
// the fields describing it are guarded. Rotation swaps in a whole new
// http.Client rather than mutating the live one, which keeps requests already
// in flight on the jar they started with.
type Client struct {
	mu        sync.RWMutex
	http      *http.Client
	sessionID string
	csrfToken string
}

// New builds a client. sessionID may be empty, in which case Login must be
// called before any other method.
func New(sessionID string) (*Client, error) {
	httpClient, err := newHTTPClient()
	if err != nil {
		return nil, err
	}

	c := &Client{http: httpClient, sessionID: sessionID}
	if sessionID != "" {
		setSessionCookie(httpClient, sessionID)
	}

	return c, nil
}

// newHTTPClient builds a client with its own cookie jar.
//
// Redirects must be followed: the first authenticated call is bounced back to
// itself so Instagram can issue the csrftoken, ds_user_id and mid cookies that
// accompany the session.
func newHTTPClient() (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create cookie jar: %w", err)
	}

	return &http.Client{Jar: jar, Timeout: requestTimeout}, nil
}

func setSessionCookie(httpClient *http.Client, sessionID string) {
	u, _ := url.Parse(baseURL)
	httpClient.Jar.SetCookies(u, []*http.Cookie{
		{Name: "sessionid", Value: sessionID, Domain: ".instagram.com", Path: "/"},
	})
}

// SessionID returns the session cookie the client is currently using.
func (c *Client) SessionID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.sessionID
}

// SetSession swaps in a new session cookie so a rotated cookie takes effect
// without a restart. The jar is rebuilt from scratch: csrftoken, ds_user_id and
// mid belong to the previous session, and sending them alongside a new
// sessionid gets the request rejected.
func (c *Client) SetSession(sessionID string) error {
	err := validateSessionID(sessionID)
	if err != nil {
		return err
	}

	httpClient, err := newHTTPClient()
	if err != nil {
		return err
	}
	setSessionCookie(httpClient, sessionID)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.http = httpClient
	c.sessionID = sessionID
	c.csrfToken = ""

	return nil
}

// snapshot returns the current transport and CSRF token together, so a request
// cannot be built from a half-rotated session.
func (c *Client) snapshot() (*http.Client, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	csrf := c.csrfToken
	if csrf == "" {
		csrf = cookieFrom(c.http, "csrftoken")
	}

	return c.http, csrf
}

func cookieFrom(httpClient *http.Client, name string) string {
	u, _ := url.Parse(baseURL)
	for _, ck := range httpClient.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}

	return ""
}

// get performs an authenticated GET against the web private API and decodes the
// JSON body into out.
func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	httpClient, csrf := c.snapshot()
	decorate(req, csrf)

	res, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxBodySize))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	err = statusError(res.StatusCode, body)
	if err != nil {
		return err
	}

	// An API path that answers with HTML has redirected to the login page,
	// which is how a rejected session surfaces.
	if !strings.Contains(res.Header.Get("content-type"), "json") {
		return fmt.Errorf("%w: %s returned %s", domain.ErrUnauthorized, path, res.Header.Get("content-type"))
	}

	err = json.Unmarshal(body, out)
	if err != nil {
		return fmt.Errorf("%w: decode %s: %v", domain.ErrBadResponse, path, err)
	}

	return nil
}

func decorate(req *http.Request, csrf string) {
	req.Header.Set("user-agent", userAgent)
	req.Header.Set("x-ig-app-id", appID)
	req.Header.Set("x-requested-with", "XMLHttpRequest")
	req.Header.Set("accept", "*/*")
	req.Header.Set("referer", baseURL+"/")

	if csrf != "" {
		req.Header.Set("x-csrftoken", csrf)
	}
}

// validateSessionID checks the shape of a session cookie before it is trusted,
// so a mistyped value is rejected at the point it is supplied rather than
// surfacing later as an authentication failure.
func validateSessionID(sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("%w: session cookie is empty", domain.ErrUnauthorized)
	}

	_, err := accountPK(sessionID)

	return err
}

// accountPK extracts the account id, which is the first field of the cookie.
func accountPK(sessionID string) (string, error) {
	// Browsers store the cookie percent-encoded; tolerate either form.
	decoded, err := url.QueryUnescape(sessionID)
	if err != nil {
		decoded = sessionID
	}

	pk, _, found := strings.Cut(decoded, ":")
	if !found || pk == "" {
		return "", fmt.Errorf("%w: session cookie has no account id", domain.ErrUnauthorized)
	}
	for _, r := range pk {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("%w: session cookie account id %q is not numeric", domain.ErrUnauthorized, pk)
		}
	}

	return pk, nil
}

// statusError maps an HTTP status onto a domain sentinel. Instagram answers
// with 200 far more often than it should, so the body is inspected too.
//
// Every branch carries the status and a body excerpt: an "unauthorized" with no
// detail is impossible to tell apart from a rate limit dressed up as a login
// wall, which is exactly the ambiguity that matters when the session works from
// one network and not another.
func statusError(status int, body []byte) error {
	// Instagram reports throttling as "please wait a few minutes", attached to
	// a 401 with require_login set. Taken at face value that reads as a dead
	// session and provokes a password login, which earns a challenge and makes
	// things worse. The message is the only reliable signal, so it wins over
	// the status code.
	if strings.Contains(string(body), "wait a few minutes") {
		return fmt.Errorf("%w: status %d: %s", domain.ErrRateLimited, status, truncate(string(body), 200))
	}

	switch {
	case status == http.StatusOK:
		if strings.Contains(string(body), "checkpoint_required") {
			return fmt.Errorf("%w: status 200: %s", domain.ErrCheckpointRequired, truncate(string(body), 300))
		}
		if strings.Contains(string(body), "login_required") {
			return fmt.Errorf("%w: status 200 login_required: %s", domain.ErrUnauthorized, truncate(string(body), 300))
		}
		return nil
	case status == http.StatusNotFound:
		return domain.ErrNotFound
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return fmt.Errorf("%w: status %d: %s", domain.ErrUnauthorized, status, truncate(string(body), 300))
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("%w: status 429: %s", domain.ErrRateLimited, truncate(string(body), 300))
	default:
		return fmt.Errorf("%w: status %d: %s", domain.ErrBadResponse, status, truncate(string(body), 300))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
