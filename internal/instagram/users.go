package instagram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/arvlas/instalker/internal/domain"
)

// followingPageSize is how many follows are requested per page.
const followingPageSize = 50

// Profile resolves a username to the account behind it.
//
// It reads the id out of the profile page rather than calling
// web_profile_info, which Instagram throttles hard. The same page carries the
// GraphQL tokens, so the load is not wasted: it primes the posts query.
func (c *Client) Profile(ctx context.Context, username string) (domain.User, error) {
	page, err := c.page(ctx, "/"+url.PathEscape(username)+"/")
	if err != nil {
		return domain.User{}, fmt.Errorf("profile %s: %w", username, err)
	}

	pk := between(page, `"profile_id":"`, `"`)
	if pk == "" {
		return domain.User{}, fmt.Errorf("profile %s: %w", username, domain.ErrNotFound)
	}

	tokens, err := parseTokens(page)
	if err != nil {
		return domain.User{}, fmt.Errorf("profile %s: %w", username, err)
	}

	c.mu.Lock()
	tokens.docID = c.tokens.docID
	c.tokens = tokens
	c.mu.Unlock()

	return domain.User{
		PK:        pk,
		Username:  username,
		IsPrivate: strings.Contains(page, `"is_private":true`),
	}, nil
}

// Following lists every account the given user follows.
func (c *Client) Following(ctx context.Context, pk string) ([]domain.User, error) {
	var users []domain.User
	maxID := ""

	for {
		var parsed struct {
			Users []struct {
				PK        json.Number `json:"pk"`
				Username  string      `json:"username"`
				FullName  string      `json:"full_name"`
				IsPrivate bool        `json:"is_private"`
			} `json:"users"`
			NextMaxID string `json:"next_max_id"`
		}

		path := fmt.Sprintf("/api/v1/friendships/%s/following/?count=%d", url.PathEscape(pk), followingPageSize)
		if maxID != "" {
			path += "&max_id=" + url.QueryEscape(maxID)
		}

		err := c.get(ctx, path, &parsed)
		if err != nil {
			return nil, fmt.Errorf("following %s: %w", pk, err)
		}

		for _, u := range parsed.Users {
			users = append(users, domain.User{
				PK:        u.PK.String(),
				Username:  u.Username,
				FullName:  u.FullName,
				IsPrivate: u.IsPrivate,
			})
		}

		if parsed.NextMaxID == "" {
			return users, nil
		}
		maxID = parsed.NextMaxID
	}
}
