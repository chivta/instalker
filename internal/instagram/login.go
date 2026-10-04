package instagram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/arvlas/instalker/internal/domain"
)

// encPasswordPrefix is the plaintext ("version 0") password envelope the web
// login endpoint still accepts.
const encPasswordPrefix = "#PWD_INSTAGRAM_BROWSER:0:"

type loginResponse struct {
	Authenticated bool   `json:"authenticated"`
	Message       string `json:"message"`
	User          bool   `json:"user"`
	CheckpointURL string `json:"checkpoint_url"`
}

// Login authenticates with username and password and stores the resulting
// session cookie on the client.
//
// Instagram frequently answers with a challenge instead of a session; that case
// surfaces as domain.ErrCheckpointRequired and can only be cleared by a human
// completing the challenge in a browser.
func (c *Client) Login(ctx context.Context, username, password string) error {
	// A login runs when the old session is dead. Starting from an empty jar keeps
	// its leftover cookies out of the request.
	err := c.reset()
	if err != nil {
		return err
	}

	c.primeCSRF()

	form := url.Values{}
	form.Set("username", username)
	form.Set("enc_password", fmt.Sprintf("%s%d:%s", encPasswordPrefix, time.Now().Unix(), password))
	form.Set("queryParams", "{}")
	form.Set("optIntoOneTap", "false")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/web/accounts/login/ajax/", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build login request: %w", err)
	}
	httpClient, csrf, claim := c.snapshot()
	decorate(req, csrf, claim)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("referer", baseURL+"/accounts/login/")

	res, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do login request: %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxBodySize))
	if err != nil {
		return fmt.Errorf("read login body: %w", err)
	}

	if res.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("%w: login answered 429", domain.ErrRateLimited)
	}

	var parsed loginResponse
	err = json.Unmarshal(body, &parsed)
	if err != nil {
		return fmt.Errorf("%w: decode login (status %d): %v", domain.ErrBadResponse, res.StatusCode, err)
	}

	switch {
	case parsed.Authenticated:
		session := cookieFrom(httpClient, sessionCookie)
		if session == "" {
			return fmt.Errorf("%w: login succeeded without a session cookie", domain.ErrBadResponse)
		}
		c.mu.Lock()
		c.sessionID = session
		c.mu.Unlock()
		return nil
	case parsed.Message == "checkpoint_required" || parsed.CheckpointURL != "":
		return fmt.Errorf("%w: complete the challenge at %s%s", domain.ErrCheckpointRequired, baseURL, parsed.CheckpointURL)
	default:
		return fmt.Errorf("%w: %s", domain.ErrUnauthorized, truncate(string(body), 300))
	}
}

// SessionUser reports who the current session belongs to.
//
// The account's primary key is the first field of the session cookie itself
// ("<pk>:<token>:..."), so this costs no request. That matters: the endpoint
// that used to be called here answers with HTML often enough that a healthy
// session looked rejected, and every extra call is one more against whatever
// budget Instagram is throttling on.
func (c *Client) SessionUser() (domain.User, error) {
	if c.SessionID() == "" {
		return domain.User{}, domain.ErrUnauthorized
	}

	pk, err := accountPK(c.SessionID())
	if err != nil {
		return domain.User{}, err
	}

	return domain.User{PK: pk}, nil
}

// primeCSRF installs a generated CSRF token for the login request. The login
// endpoint only checks that cookie and header match. Loading /accounts/login/
// to get a token from Instagram is not needed, and Instagram answers that page
// with 429 to an address it has seen many logged-out requests from.
func (c *Client) primeCSRF() {
	csrf := newCSRFToken()

	httpClient, _, _ := c.snapshot()
	u, _ := url.Parse(baseURL)
	httpClient.Jar.SetCookies(u, []*http.Cookie{
		{Name: csrfCookie, Value: csrf, Domain: ".instagram.com", Path: "/"},
	})

	c.mu.Lock()
	c.csrfToken = csrf
	c.mu.Unlock()
}
