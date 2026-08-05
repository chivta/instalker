package instagram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/arvlas/instalker/internal/domain"
)

// followingPageSize is how many follows are requested per page.
const followingPageSize = 50

// Profile resolves a username to the account behind it.
func (c *Client) Profile(ctx context.Context, username string) (domain.User, error) {
	var parsed struct {
		Data struct {
			User *struct {
				ID        string `json:"id"`
				Username  string `json:"username"`
				FullName  string `json:"full_name"`
				IsPrivate bool   `json:"is_private"`
			} `json:"user"`
		} `json:"data"`
	}

	err := c.get(ctx, "/api/v1/users/web_profile_info/?username="+url.QueryEscape(username), &parsed)
	if err != nil {
		return domain.User{}, fmt.Errorf("profile %s: %w", username, err)
	}
	if parsed.Data.User == nil {
		return domain.User{}, fmt.Errorf("profile %s: %w", username, domain.ErrNotFound)
	}

	return domain.User{
		PK:        parsed.Data.User.ID,
		Username:  parsed.Data.User.Username,
		FullName:  parsed.Data.User.FullName,
		IsPrivate: parsed.Data.User.IsPrivate,
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
