package instagram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	asbdID    = "359341"
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

	// sessionCookie is the cookie Instagram deletes when it logs a session out.
	sessionCookie = "sessionid"
	csrfCookie    = "csrftoken"
	// csrfTokenBytes gives the 32 hex digits Instagram's own tokens have.
	csrfTokenBytes = 16
	// claimHeader carries a token Instagram hands out and expects echoed back.
	claimHeader = "x-ig-set-www-claim"
	// defaultClaim is what the web client sends before it has been given one.
	defaultClaim = "0"

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
	claim     string
	tokens    pageTokens
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

// setSessionCookie installs a session along with a CSRF token of our own.
// Logged-in pages never issue a csrftoken cookie, and Instagram refuses a
// GraphQL POST without one; it only checks that cookie and header match.
func setSessionCookie(httpClient *http.Client, sessionID string) {
	u, _ := url.Parse(baseURL)
	httpClient.Jar.SetCookies(u, []*http.Cookie{
		{Name: sessionCookie, Value: sessionID, Domain: ".instagram.com", Path: "/"},
		{Name: csrfCookie, Value: newCSRFToken(), Domain: ".instagram.com", Path: "/"},
	})
}

// newCSRFToken returns 32 random hex digits, the shape Instagram issues.
func newCSRFToken() string {
	buf := make([]byte, csrfTokenBytes)
	_, _ = rand.Read(buf)

	return hex.EncodeToString(buf)
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
	c.claim = ""
	c.tokens = pageTokens{}

	return nil
}

// reset drops the session and every cookie that came with it.
func (c *Client) reset() error {
	httpClient, err := newHTTPClient()
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.http = httpClient
	c.sessionID = ""
	c.csrfToken = ""
	c.claim = ""
	c.tokens = pageTokens{}

	return nil
}

// snapshot returns the current transport and CSRF token together, so a request
// cannot be built from a half-rotated session.
func (c *Client) snapshot() (*http.Client, string, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	csrf := c.csrfToken
	if csrf == "" {
		csrf = cookieFrom(c.http, csrfCookie)
	}

	claim := c.claim
	if claim == "" {
		claim = defaultClaim
	}

	return c.http, csrf, claim
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

	body, err := c.send(req)
	if err != nil {
		return err
	}

	err = json.Unmarshal(body, out)
	if err != nil {
		return fmt.Errorf("%w: decode %s: %v", domain.ErrBadResponse, path, err)
	}

	return nil
}

// send performs an API request with browser headers and returns the JSON body,
// mapping every way Instagram signals trouble onto a domain sentinel.
func (c *Client) send(req *http.Request) ([]byte, error) {
	httpClient, csrf, claim := c.snapshot()
	decorate(req, csrf, claim)

	hadSession := cookieFrom(httpClient, sessionCookie) != ""

	res, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxBodySize))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if issued := res.Header.Get(claimHeader); issued != "" {
		c.mu.Lock()
		c.claim = issued
		c.mu.Unlock()
	}

	// Instagram logs a session out by expiring its cookie in the response. From
	// then on it answers as if to an anonymous visitor, which is easy to mistake
	// for throttling, so this is checked before anything else.
	if hadSession && cookieFrom(httpClient, sessionCookie) == "" {
		return nil, fmt.Errorf("%w: instagram expired the session cookie", domain.ErrUnauthorized)
	}

	err = statusError(res.StatusCode, body)
	if err != nil {
		return nil, err
	}

	// An API call answered with HTML was redirected to a web page, which is how
	// Instagram refuses a call it does not serve to this session.
	if trimmed := strings.TrimSpace(string(body)); !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return nil, fmt.Errorf("%w: %s answered with a web page", domain.ErrUnauthorized, req.URL.Path)
	}

	return body, nil
}

// decorate sets the headers the web client sends with its API calls. Instagram
// answers a request missing them differently from one carrying them.
func decorate(req *http.Request, csrf, claim string) {
	req.Header.Set("user-agent", userAgent)
	req.Header.Set("x-ig-app-id", appID)
	req.Header.Set("x-asbd-id", asbdID)
	req.Header.Set("x-ig-www-claim", claim)
	req.Header.Set("x-requested-with", "XMLHttpRequest")
	req.Header.Set("accept", "*/*")
	req.Header.Set("accept-language", "en-US,en;q=0.9")
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-site", "same-origin")
	if req.Header.Get("referer") == "" {
		req.Header.Set("referer", baseURL+"/")
	}

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
func statusError(status int, body []byte) error {
	text := string(body)

	// "require_login" means Instagram is treating the request as anonymous. It
	// comes with a "please wait a few minutes" message that reads like
	// throttling, which hid a dead session for a month. Real throttling of a
	// live session arrives as a 429.
	if strings.Contains(text, `"require_login":true`) || strings.Contains(text, "login_required") {
		return fmt.Errorf("%w: status %d: %s", domain.ErrUnauthorized, status, truncate(text, 200))
	}
	if strings.Contains(text, "checkpoint_required") {
		return fmt.Errorf("%w: status %d: %s", domain.ErrCheckpointRequired, status, truncate(text, 300))
	}

	switch status {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return domain.ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: status %d: %s", domain.ErrUnauthorized, status, truncate(text, 300))
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: status 429: %s", domain.ErrRateLimited, truncate(text, 300))
	default:
		if strings.Contains(text, "wait a few minutes") {
			return fmt.Errorf("%w: status %d: %s", domain.ErrRateLimited, status, truncate(text, 200))
		}
		return fmt.Errorf("%w: status %d: %s", domain.ErrBadResponse, status, truncate(text, 300))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
