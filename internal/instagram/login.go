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

// VerifySession checks that the current session cookie is still accepted.
func (c *Client) VerifySession(ctx context.Context) (domain.User, error) {
	if c.sessionID == "" {
		return domain.User{}, domain.ErrUnauthorized
	}

	var me struct {
		FormData struct {
			Username  string `json:"username"`
			FirstName string `json:"first_name"`
		} `json:"form_data"`
	}

	err := c.get(ctx, "/api/v1/accounts/edit/web_form_data/", &me)
	if err != nil {
		return domain.User{}, fmt.Errorf("verify session: %w", err)
	}
	if me.FormData.Username == "" {
		return domain.User{}, domain.ErrUnauthorized
	}

	// The edit form carries no primary key; resolve it from the public profile.
	user, err := c.Profile(ctx, me.FormData.Username)
	if err != nil {
		return domain.User{}, fmt.Errorf("verify session: %w", err)
	}

	return user, nil
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
