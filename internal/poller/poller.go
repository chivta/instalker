package poller

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/arvlas/instalker/internal/domain"
	"github.com/arvlas/instalker/internal/metrics"
)

const (
	// sendDelay spaces out Telegram deliveries so a burst of new media does not
	// trip the bot API rate limit.
	sendDelay = 2 * time.Second

	// targetGap spaces out the accounts within one cycle.
	targetGap = 5 * time.Second

	// pollBackoffFactor and maxPollInterval bound how far a throttled poller
	// backs off. Instagram lifts these blocks on its own; continuing to poll
	// through one only keeps it alive.
	pollBackoffFactor = 2
	maxPollInterval   = time.Hour
)

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

// Run polls until ctx is cancelled. A failed cycle is logged and retried later
// rather than bringing the process down.
//
// The interval is not fixed: polling on schedule through a throttle is what
// keeps the throttle alive, so a rate-limited cycle backs the next one off and a
// clean cycle restores the configured pace.
func (p *Poller) Run(ctx context.Context) error {
	delay := p.interval

	for {
		throttled := p.cycle(ctx)

		switch {
		case throttled:
			delay = min(delay*pollBackoffFactor, maxPollInterval)
			log.Warn().Dur("next_poll_in", delay).Msg("instagram is throttling, backing off")
		case delay != p.interval:
			delay = p.interval
			log.Info().Dur("next_poll_in", delay).Msg("throttling cleared, resuming the configured interval")
		}

		select {
		case <-ctx.Done():
			log.Info().Msg("poller stopped")
			return nil
		case <-time.After(delay):
		}
	}
}

// cycle polls every target once, reporting whether Instagram throttled it.
func (p *Poller) cycle(ctx context.Context) bool {
	var blocked error
	throttled := false

	for i, target := range p.targets {
		if ctx.Err() != nil {
			return throttled
		}

		// Space the targets out. Firing every request back to back is what a
		// scraper looks like; a few seconds between them costs nothing when the
		// interval is minutes.
		if i > 0 {
			sleep(ctx, jitter(targetGap))
		}

		err := p.pollTarget(ctx, target)
		if err != nil {
			log.Error().Err(err).Str("target", target.Username).Msg("poll cycle failed for target")

			if errors.Is(err, domain.ErrRateLimited) {
				throttled = true
			}
			if blocked == nil && isBlockedErr(err) {
				blocked = err
			}
		}
	}

	p.reportStall(ctx, blocked)
	metrics.IncPollCycle()

	return throttled
}

// jitter spreads a delay by up to a quarter either way, so repeated cycles do
// not settle into a fixed, obviously mechanical rhythm.
func jitter(d time.Duration) time.Duration {
	spread := int64(d) / 2

	return d - time.Duration(spread/2) + time.Duration(rand.Int64N(spread+1))
}

// reportStall tells the chat when Instagram stops answering, and again when it
// recovers. Without this the bot polls into the void: the logs fill up but
// nobody is watching them.
func (p *Poller) reportStall(ctx context.Context, blocked error) {
	switch {
	case blocked != nil && !p.authAlerted:
		p.authAlerted = true

		text := "🔴 instalker: Instagram is refusing the session, so polling is stalled.\n\n" +
			"Log in to instagram.com in a browser and send the fresh cookie here as " +
			"<code>/session &lt;sessionid&gt;</code>."
		if errors.Is(blocked, domain.ErrRateLimited) {
			// Rotating the session would not help here, so do not ask for one.
			text = "🔴 instalker: Instagram is rate limiting this host, so polling is stalled. " +
				"It may clear on its own; if it persists the requests need to come from a different network."
		}

		err := p.sender.Notify(ctx, text)
		if err != nil {
			log.Error().Err(err).Msg("failed to send stall alert")
		}
	case blocked == nil && p.authAlerted:
		p.authAlerted = false
		err := p.sender.Notify(ctx, "🟢 instalker: Instagram is answering again, polling resumed.")
		if err != nil {
			log.Error().Err(err).Msg("failed to send recovery notice")
		}
	}
}

func isBlockedErr(err error) bool {
	return isAuthErr(err) || errors.Is(err, domain.ErrRateLimited)
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

	// One failed feed is still worth returning when Instagram is the reason:
	// that means the session or the host is blocked, not that the feed is empty.
	if isBlockedErr(postsErr) || isBlockedErr(storiesErr) {
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

// Probe scrapes every target once and reports what came back, without
// delivering anything or touching the seen-state. It answers the question the
// logs otherwise only answer on the next tick: is scraping working right now.
func (p *Poller) Probe(ctx context.Context) domain.Probe {
	start := time.Now()
	probe := domain.Probe{Targets: make([]domain.TargetProbe, 0, len(p.targets))}

	for _, target := range p.targets {
		result := domain.TargetProbe{User: target}

		posts, err := p.insta.Posts(ctx, target)
		if err != nil {
			result.Err = err
		} else {
			result.Posts = len(posts)
			result.Latest = newest(posts)
		}

		stories, err := p.insta.Stories(ctx, target)
		switch {
		case err != nil && result.Err == nil:
			result.Err = err
		case err == nil:
			result.Stories = len(stories)
			if storyLatest := newest(stories); storyLatest.After(result.Latest) {
				result.Latest = storyLatest
			}
		}

		probe.Targets = append(probe.Targets, result)
	}

	probe.Elapsed = time.Since(start)

	return probe
}

func newest(media []domain.Media) time.Time {
	var latest time.Time
	for _, m := range media {
		if m.TakenAt.After(latest) {
			latest = m.TakenAt
		}
	}

	return latest
}
