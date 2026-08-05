package poller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/arvlas/instalker/internal/domain"
	"github.com/arvlas/instalker/internal/metrics"
)

// sendDelay spaces out Telegram deliveries so a burst of new media does not
// trip the bot API rate limit.
const sendDelay = 2 * time.Second

type instagramClient interface {
	Profile(ctx context.Context, username string) (domain.User, error)
	Following(ctx context.Context, pk string) ([]domain.User, error)
	Posts(ctx context.Context, owner domain.User) ([]domain.Media, error)
	Stories(ctx context.Context, owner domain.User) ([]domain.Media, error)
}

type mediaRepo interface {
	Seen(ctx context.Context, ownerPK string, kind domain.Kind, mediaID string) (bool, error)
	MarkSeen(ctx context.Context, media domain.Media) error
	Initialized(ctx context.Context, ownerPK string) (bool, error)
	MarkInitialized(ctx context.Context, user domain.User) error
}

type sender interface {
	Send(ctx context.Context, media domain.Media) error
}

// Poller watches a fixed set of Instagram accounts and forwards anything new.
type Poller struct {
	insta    instagramClient
	repo     mediaRepo
	sender   sender
	targets  []domain.User
	interval time.Duration
}

// New builds a poller over an already resolved set of targets.
func New(insta instagramClient, repo mediaRepo, sender sender, targets []domain.User, interval time.Duration) *Poller {
	return &Poller{
		insta:    insta,
		repo:     repo,
		sender:   sender,
		targets:  targets,
		interval: interval,
	}
}

// Run polls until ctx is cancelled. A failed cycle is logged and retried on the
// next tick rather than bringing the process down.
func (p *Poller) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	p.cycle(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("poller stopped")
			return nil
		case <-ticker.C:
			p.cycle(ctx)
		}
	}
}

func (p *Poller) cycle(ctx context.Context) {
	for _, target := range p.targets {
		if ctx.Err() != nil {
			return
		}

		err := p.pollTarget(ctx, target)
		if err != nil {
			log.Error().Err(err).Str("target", target.Username).Msg("poll cycle failed for target")
		}
	}

	metrics.IncPollCycle()
}

func (p *Poller) pollTarget(ctx context.Context, target domain.User) error {
	initialized, err := p.repo.Initialized(ctx, target.PK)
	if err != nil {
		return err
	}

	posts, postsErr := p.insta.Posts(ctx, target)
	if postsErr != nil {
		log.Error().Err(postsErr).Str("target", target.Username).Msg("failed to fetch posts")
	}

	stories, storiesErr := p.insta.Stories(ctx, target)
	if storiesErr != nil {
		log.Error().Err(storiesErr).Str("target", target.Username).Msg("failed to fetch stories")
	}

	if postsErr != nil && storiesErr != nil {
		return fmt.Errorf("both feeds failed: %w", errors.Join(postsErr, storiesErr))
	}

	// Instagram returns newest first; deliver in chronological order.
	media := append(reverse(posts), reverse(stories)...)

	fresh := 0
	for _, m := range media {
		seen, err := p.repo.Seen(ctx, m.Owner.PK, m.Kind, m.ID)
		if err != nil {
			return err
		}
		if seen {
			continue
		}
		fresh++

		// The first cycle only establishes a baseline, otherwise starting the
		// bot would replay the entire visible history into the chat.
		if initialized {
			err = p.sender.Send(ctx, m)
			if err != nil {
				log.Error().Err(err).Str("target", target.Username).Str("media_id", m.ID).Msg("failed to deliver media")
				continue
			}
			metrics.IncMediaDelivered()
			log.Info().Str("target", target.Username).Str("kind", string(m.Kind)).Str("media_id", m.ID).Msg("delivered media")
			sleep(ctx, sendDelay)
		}

		err = p.repo.MarkSeen(ctx, m)
		if err != nil {
			return err
		}
	}

	if !initialized {
		err = p.repo.MarkInitialized(ctx, target)
		if err != nil {
			return err
		}
		log.Info().Str("target", target.Username).Int("baselined", fresh).Msg("baseline established, future media will be forwarded")
	}

	return nil
}

func reverse(media []domain.Media) []domain.Media {
	out := make([]domain.Media, 0, len(media))
	for i := len(media) - 1; i >= 0; i-- {
		out = append(out, media[i])
	}

	return out
}

func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
