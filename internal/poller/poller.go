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
	Notify(ctx context.Context, text string) error
}

// Poller watches a fixed set of Instagram accounts and forwards anything new.
type Poller struct {
	insta    instagramClient
	repo     mediaRepo
	sender   sender
	targets  []domain.User
	interval time.Duration

	// authAlerted keeps a broken session from re-alerting on every tick. It is
	// only touched from the single goroutine running Run.
	authAlerted bool
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
	authBroken := false

	for _, target := range p.targets {
		if ctx.Err() != nil {
			return
		}

		err := p.pollTarget(ctx, target)
		if err != nil {
			log.Error().Err(err).Str("target", target.Username).Msg("poll cycle failed for target")

			if errors.Is(err, domain.ErrUnauthorized) || errors.Is(err, domain.ErrCheckpointRequired) {
				authBroken = true
			}
		}
	}

	p.reportAuthState(ctx, authBroken)
	metrics.IncPollCycle()
}

// reportAuthState tells the chat when the Instagram session stops being
// accepted, and again when it recovers. Without this the bot polls into the
// void: the logs fill up but nobody is watching them.
func (p *Poller) reportAuthState(ctx context.Context, authBroken bool) {
	switch {
	case authBroken && !p.authAlerted:
		p.authAlerted = true
		err := p.sender.Notify(ctx, "🔴 instalker: Instagram is refusing the session, so polling is stalled.\n\n"+
			"Log in to instagram.com in a browser, copy a fresh <code>sessionid</code> cookie into the "+
			"<code>IG_SESSIONID</code> secret and restart the bot.")
		if err != nil {
			log.Error().Err(err).Msg("failed to send auth alert")
		}
	case !authBroken && p.authAlerted:
		p.authAlerted = false
		err := p.sender.Notify(ctx, "🟢 instalker: Instagram session accepted again, polling resumed.")
		if err != nil {
			log.Error().Err(err).Msg("failed to send auth recovery notice")
		}
	}
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

	// Baselining off a half-fetched target would mark only what was reachable as
	// seen, and the missing feed would later arrive as a flood of "new" media.
	if !initialized && postsErr == nil && storiesErr == nil {
		err = p.repo.MarkInitialized(ctx, target)
		if err != nil {
			return err
		}
		log.Info().Str("target", target.Username).Int("baselined", fresh).Msg("baseline established, future media will be forwarded")
	}

	// One failed feed is still worth returning when it failed on auth: that
	// means the session is broken, not that the feed was empty.
	if isAuthErr(postsErr) || isAuthErr(storiesErr) {
		return errors.Join(postsErr, storiesErr)
	}

	return nil
}

func isAuthErr(err error) bool {
	return errors.Is(err, domain.ErrUnauthorized) || errors.Is(err, domain.ErrCheckpointRequired)
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
