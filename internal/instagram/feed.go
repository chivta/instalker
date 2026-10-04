package instagram

import (
	"context"
	"fmt"
	"net/url"

	"github.com/arvlas/instalker/internal/domain"
)

// postsPageSize is how many recent posts are pulled per poll.
const postsPageSize = 12

// Stories returns the currently live story items of the given user.
func (c *Client) Stories(ctx context.Context, owner domain.User) ([]domain.Media, error) {
	var parsed struct {
		// A pointer, because an anonymous visitor gets {"reels":{}} with no
		// reels_media key at all, while a logged-in one gets the key even when
		// there are no stories.
		ReelsMedia *[]struct {
			Items []item `json:"items"`
		} `json:"reels_media"`
	}

	path := "/api/v1/feed/reels_media/?reel_ids=" + url.QueryEscape(owner.PK)
	err := c.get(ctx, path, &parsed)
	if err != nil {
		return nil, fmt.Errorf("stories %s: %w", owner.Username, err)
	}
	if parsed.ReelsMedia == nil {
		return nil, fmt.Errorf("stories %s: %w: answered as to a logged-out visitor", owner.Username, domain.ErrUnauthorized)
	}

	var media []domain.Media
	for _, reel := range *parsed.ReelsMedia {
		for _, it := range reel.Items {
			media = append(media, it.toMedia(domain.KindStory, owner))
		}
	}

	return media, nil
}
