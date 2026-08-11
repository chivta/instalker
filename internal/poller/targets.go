package poller

import (
	"context"
	"fmt"

	"github.com/rs/zerolog/log"

	"github.com/arvlas/instalker/internal/domain"
)

// ResolveTargets determines which accounts to watch. Explicit usernames win;
// otherwise the accounts the logged-in user follows are used.
func ResolveTargets(ctx context.Context, insta instagramClient, me domain.User, usernames []string) ([]domain.User, error) {
	if len(usernames) > 0 {
		targets := make([]domain.User, 0, len(usernames))
		for _, username := range usernames {
			user, err := insta.Profile(ctx, username)
			if err != nil {
				// Both verbs are %w: callers match on ErrTargetsUnresolved to
				// describe the failure and on the cause to decide whether it is
				// worth retrying.
				return nil, fmt.Errorf("%w: %w", domain.ErrTargetsUnresolved, err)
			}
			targets = append(targets, user)
		}

		return targets, nil
	}

	following, err := insta.Following(ctx, me.PK)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrTargetsUnresolved, err)
	}
	if len(following) == 0 {
		return nil, fmt.Errorf("%w: %s follows nobody and TARGETS is empty", domain.ErrTargetsUnresolved, me.Username)
	}

	log.Info().Int("count", len(following)).Msg("resolved targets from following list")

	return following, nil
}
