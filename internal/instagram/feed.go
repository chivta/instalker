package instagram

import (
	"context"
	"fmt"
	"net/url"

	"github.com/arvlas/instalker/internal/domain"
)

// postsPageSize is how many recent posts are pulled per poll.
const postsPageSize = 12

// Posts returns the most recent posts of the given user, newest first.
func (c *Client) Posts(ctx context.Context, owner domain.User) ([]domain.Media, error) {
	var parsed struct {
		Items []item `json:"items"`
	}

	path := fmt.Sprintf("/api/v1/feed/user/%s/?count=%d", url.PathEscape(owner.PK), postsPageSize)
	err := c.get(ctx, path, &parsed)
	if err != nil {
		return nil, fmt.Errorf("posts %s: %w", owner.Username, err)
	}

	media := make([]domain.Media, 0, len(parsed.Items))
	for _, it := range parsed.Items {
		media = append(media, it.toMedia(domain.KindPost, owner))
	}

	return media, nil
}

// Stories returns the currently live story items of the given user.
func (c *Client) Stories(ctx context.Context, owner domain.User) ([]domain.Media, error) {
	var parsed struct {
		ReelsMedia []struct {
			Items []item `json:"items"`
		} `json:"reels_media"`
	}

	path := "/api/v1/feed/reels_media/?reel_ids=" + url.QueryEscape(owner.PK)
	err := c.get(ctx, path, &parsed)
	if err != nil {
		return nil, fmt.Errorf("stories %s: %w", owner.Username, err)
	}

	var media []domain.Media
	for _, reel := range parsed.ReelsMedia {
		for _, it := range reel.Items {
			media = append(media, it.toMedia(domain.KindStory, owner))
		}
	}

	return media, nil
}
