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
	err := c.primeCSRF(ctx)
	if err != nil {
		return err
	}

	form := url.Values{}
	form.Set("username", username)
	form.Set("enc_password", fmt.Sprintf("%s%d:%s", encPasswordPrefix, time.Now().Unix(), password))
	form.Set("queryParams", "{}")
	form.Set("optIntoOneTap", "false")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/web/accounts/login/ajax/", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build login request: %w", err)
	}
	c.decorate(req)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("referer", baseURL+"/accounts/login/")

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("do login request: %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxBodySize))
	if err != nil {
		return fmt.Errorf("read login body: %w", err)
	}

	var parsed loginResponse
	err = json.Unmarshal(body, &parsed)
	if err != nil {
		return fmt.Errorf("%w: decode login: %v", domain.ErrBadResponse, err)
	}

	switch {
	case parsed.Authenticated:
		session := c.cookie("sessionid")
		if session == "" {
			return fmt.Errorf("%w: login succeeded without a session cookie", domain.ErrBadResponse)
		}
		c.sessionID = session
		return nil
	case parsed.Message == "checkpoint_required" || parsed.CheckpointURL != "":
		return fmt.Errorf("%w: complete the challenge at %s%s", domain.ErrCheckpointRequired, baseURL, parsed.CheckpointURL)
	case res.StatusCode == http.StatusTooManyRequests:
		return domain.ErrRateLimited
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
	if c.sessionID == "" {
		return domain.User{}, domain.ErrUnauthorized
	}

	// Browsers store the cookie percent-encoded; tolerate either form.
	decoded, err := url.QueryUnescape(c.sessionID)
	if err != nil {
		decoded = c.sessionID
	}

	pk, _, found := strings.Cut(decoded, ":")
	if !found || pk == "" {
		return domain.User{}, fmt.Errorf("%w: session cookie has no account id", domain.ErrUnauthorized)
	}
	for _, r := range pk {
		if r < '0' || r > '9' {
			return domain.User{}, fmt.Errorf("%w: session cookie account id %q is not numeric", domain.ErrUnauthorized, pk)
		}
	}

	return domain.User{PK: pk}, nil
}

// primeCSRF fetches the login page so the jar receives a csrftoken cookie.
func (c *Client) primeCSRF(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/accounts/login/", nil)
	if err != nil {
		return fmt.Errorf("build csrf request: %w", err)
	}
	req.Header.Set("user-agent", userAgent)

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("do csrf request: %w", err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxBodySize))

	c.csrfToken = c.cookie("csrftoken")
	if c.csrfToken == "" {
		return fmt.Errorf("%w: no csrftoken cookie issued", domain.ErrBadResponse)
	}

	return nil
}
