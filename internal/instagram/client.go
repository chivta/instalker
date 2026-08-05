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
type Client struct {
	http      *http.Client
	sessionID string
	csrfToken string
}

// New builds a client. sessionID may be empty, in which case Login must be
// called before any other method.
func New(sessionID string) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create cookie jar: %w", err)
	}

	c := &Client{
		// Redirects must be followed: the first authenticated call is bounced
		// back to itself so Instagram can issue the csrftoken, ds_user_id and
		// mid cookies that accompany the session.
		http:      &http.Client{Jar: jar, Timeout: requestTimeout},
		sessionID: sessionID,
	}

	if sessionID != "" {
		c.setSessionCookies(sessionID)
	}

	return c, nil
}

// SessionID returns the session cookie the client is currently authenticated with.
func (c *Client) SessionID() string {
	return c.sessionID
}

func (c *Client) setSessionCookies(sessionID string) {
	u, _ := url.Parse(baseURL)
	c.sessionID = sessionID
	c.http.Jar.SetCookies(u, []*http.Cookie{
		{Name: "sessionid", Value: sessionID, Domain: ".instagram.com", Path: "/"},
	})
}

func (c *Client) cookie(name string) string {
	u, _ := url.Parse(baseURL)
	for _, ck := range c.http.Jar.Cookies(u) {
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
	c.decorate(req)

	res, err := c.http.Do(req)
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

func (c *Client) decorate(req *http.Request) {
	req.Header.Set("user-agent", userAgent)
	req.Header.Set("x-ig-app-id", appID)
	req.Header.Set("x-requested-with", "XMLHttpRequest")
	req.Header.Set("accept", "*/*")
	req.Header.Set("referer", baseURL+"/")

	csrf := c.csrfToken
	if csrf == "" {
		csrf = c.cookie("csrftoken")
	}
	if csrf != "" {
		req.Header.Set("x-csrftoken", csrf)
	}
}

// statusError maps an HTTP status onto a domain sentinel. Instagram answers
// with 200 far more often than it should, so the body is inspected too.
func statusError(status int, body []byte) error {
	switch {
	case status == http.StatusOK:
		if strings.Contains(string(body), "checkpoint_required") {
			return domain.ErrCheckpointRequired
		}
		if strings.Contains(string(body), "login_required") {
			return domain.ErrUnauthorized
		}
		return nil
	case status == http.StatusNotFound:
		return domain.ErrNotFound
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return domain.ErrUnauthorized
	case status == http.StatusTooManyRequests:
		return domain.ErrRateLimited
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
