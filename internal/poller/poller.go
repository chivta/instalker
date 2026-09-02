package poller

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/arvlas/instalker/internal/domain"
	"github.com/arvlas/instalker/internal/metrics"
	"github.com/arvlas/instalker/internal/schedule"
)

const (
	// sendDelay spaces out Telegram deliveries so a burst of new media does not
	// trip the bot API rate limit.
	sendDelay = 2 * time.Second

	// targetGap spaces out the accounts within one cycle.
	targetGap = 5 * time.Second

	// tickInterval is how often the loop asks the schedule what is due. It only
	// needs to be finer than the shortest interval anyone configures.
	tickInterval = time.Minute

	// pollBackoffFactor, minBackoff and maxBackoff bound how long a throttled
	// poller pauses. Instagram lifts these blocks on its own; continuing to poll
	// through one only keeps it alive.
	pollBackoffFactor = 2
	minBackoff        = 10 * time.Minute
	maxBackoff        = time.Hour
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

// Poller watches a set of Instagram accounts and forwards anything new, at the
// cadence the schedule defines.
type Poller struct {
	insta  instagramClient
	repo   mediaRepo
	sender sender

	targets []domain.User
	plan    *schedule.Plan

	// Everything below is touched only by the single goroutine running Run.
	feeds       map[string]*feedState
	awake       bool
	authAlerted bool
}

// feedState tracks one account's one feed.
//
// Throttling is per endpoint, not per host: Instagram has blocked the timeline
// feed while serving stories perfectly well. Pausing everything on one feed's
// 401 would throw away the working one, and stories expire in a day.
type feedState struct {
	username    string
	kind        domain.Kind
	last        time.Time
	backoff     time.Duration
	pausedUntil time.Time
	// blocked holds why this feed is currently unusable, cleared only by a
	// successful fetch. Deriving the chat alert from the last tick instead made
	// it flap: a tick that polled only the healthy feed looked like recovery.
	blocked error
}

// New builds a poller over an already resolved set of targets.
func New(insta instagramClient, repo mediaRepo, sender sender, targets []domain.User, plan *schedule.Plan) *Poller {
	return &Poller{
		insta:   insta,
		repo:    repo,
		sender:  sender,
		targets: targets,
		plan:    plan,
		feeds:   map[string]*feedState{},
	}
}

// Run polls until ctx is cancelled. A failed fetch is logged and retried on a
// later tick rather than bringing the process down.
//
// The loop wakes often and asks the schedule what is due, rather than sleeping
// for a fixed interval: feeds run at different cadences per account, and only
// inside the configured window.
func (p *Poller) Run(ctx context.Context) error {
	log.Info().Str("window", p.plan.Window()).Msg("polling on schedule")

	for {
		p.tick(ctx, time.Now())

		select {
		case <-ctx.Done():
			log.Info().Msg("poller stopped")
			return nil
		case <-time.After(tickInterval):
		}
	}
}

// tick polls whatever is due at now.
func (p *Poller) tick(ctx context.Context, now time.Time) {
	if !p.plan.Active(now) {
		if p.awake {
			p.awake = false
			log.Info().Str("window", p.plan.Window()).Msg("outside the polling window, sleeping")
		}
		return
	}
	if !p.awake {
		p.awake = true
		log.Info().Msg("inside the polling window, polling resumed")
	}

	polled := 0

	for _, target := range p.targets {
		if ctx.Err() != nil {
			return
		}

		due := p.due(ctx, target, now)
		if len(due) == 0 {
			continue
		}

		// Space the accounts out. Firing every request back to back is what a
		// scraper looks like; a few seconds costs nothing at these intervals.
		if polled > 0 {
			sleep(ctx, jitter(targetGap))
		}
		polled++

		err := p.pollTarget(ctx, target, due, now)
		if err != nil {
			log.Error().Err(err).Str("target", target.Username).Msg("poll failed for target")

		}
	}

	if polled == 0 {
		return
	}

	p.reportStall(ctx)
	metrics.IncPollCycle()
}

// due reports which feeds of a target should be fetched now.
//
// A target that has never been baselined gets both feeds regardless of
// schedule: the baseline marks what is already visible as seen, and doing that
// one feed at a time would let the other arrive later as a flood of "new" media.
func (p *Poller) due(ctx context.Context, target domain.User, now time.Time) []domain.Kind {
	initialized, err := p.repo.Initialized(ctx, target.PK)
	if err != nil {
		log.Error().Err(err).Str("target", target.Username).Msg("failed to read watch state")
		return nil
	}
	if !initialized {
		return []domain.Kind{domain.KindPost, domain.KindStory}
	}

	account, found := p.plan.For(target.Username)
	if !found {
		// Not named in the schedule: fall back to the defaults it defines.
		account = schedule.Account{Posts: p.plan.Defaults().Posts, Stories: p.plan.Defaults().Stories}
	}

	var due []domain.Kind
	for _, candidate := range []struct {
		kind  domain.Kind
		every time.Duration
	}{
		{domain.KindPost, account.Posts},
		{domain.KindStory, account.Stories},
	} {
		state := p.feed(target.PK, candidate.kind)
		if now.Before(state.pausedUntil) {
			continue
		}
		if isDue(state.last, candidate.every, now) {
			due = append(due, candidate.kind)
		}
	}

	return due
}

// feed returns the state for one account's feed, creating it on first use.
func (p *Poller) feed(pk string, kind domain.Kind) *feedState {
	key := pk + "/" + string(kind)

	state, ok := p.feeds[key]
	if !ok {
		state = &feedState{kind: kind}
		p.feeds[key] = state
	}

	return state
}

// isDue reports whether a feed with the given interval should run. A zero
// interval means the feed is switched off.
func isDue(last time.Time, every time.Duration, now time.Time) bool {
	if every == 0 {
		return false
	}

	return last.IsZero() || !now.Before(last.Add(every))
}

// jitter spreads a delay by up to a quarter either way, so repeated cycles do
// not settle into a fixed, obviously mechanical rhythm.
func jitter(d time.Duration) time.Duration {
	spread := int64(d) / 2

	return d - time.Duration(spread/2) + time.Duration(rand.Int64N(spread+1))
}

// reportStall tells the chat when a feed stops working, and again when it
// recovers. Without this the bot polls into the void: the logs fill up but
// nobody is watching them.
//
// The verdict comes from the feeds' own state rather than the last tick's
// outcome. A paused feed stays blocked until it actually succeeds, so a tick
// that happened to poll only the healthy feed no longer reads as recovery —
// which had the bot announcing failure and recovery in an endless alternation.
func (p *Poller) reportStall(ctx context.Context) {
	var (
		names   []string
		cause   error
		blocked bool
	)

	for _, state := range p.feeds {
		if state.blocked == nil {
			continue
		}
		blocked = true

		names = append(names, fmt.Sprintf("%s %s", state.username, state.kind))
		// An auth failure needs a person, so it wins over a throttle when both
		// are present: it is the one with something to do about it.
		if cause == nil || (isAuthErr(state.blocked) && !isAuthErr(cause)) {
			cause = state.blocked
		}
	}

	switch {
	case blocked && !p.authAlerted:
		p.authAlerted = true
		sort.Strings(names)

		text := fmt.Sprintf("🔴 instalker: Instagram is refusing the session for %s.\n\n"+
			"Log in to instagram.com in a browser and send the fresh cookie here as "+
			"<code>/session &lt;sessionid&gt;</code>.", strings.Join(names, ", "))
		if !isAuthErr(cause) {
			// Rotating the session does not clear a throttle, and neither does
			// moving networks — the same block appears from any address.
			text = fmt.Sprintf("🔴 instalker: Instagram is rate limiting %s. "+
				"Other feeds keep running, and this one retries on its own with a growing delay.",
				strings.Join(names, ", "))
		}

		err := p.sender.Notify(ctx, text)
		if err != nil {
			log.Error().Err(err).Msg("failed to send stall alert")
		}
	case !blocked && p.authAlerted:
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

func (p *Poller) pollTarget(ctx context.Context, target domain.User, due []domain.Kind, now time.Time) error {
	initialized, err := p.repo.Initialized(ctx, target.PK)
	if err != nil {
		return err
	}

	var (
		media    []domain.Media
		failures []error
	)

	for _, kind := range due {
		fetched, err := p.fetch(ctx, target, kind)
		if err != nil {
			log.Error().Err(err).Str("target", target.Username).Str("kind", string(kind)).Msg("failed to fetch feed")
			failures = append(failures, err)

			state := p.feed(target.PK, kind)
			state.username = target.Username
			if isBlockedErr(err) {
				state.blocked = err
			}

			// Polling on schedule through a throttle is what keeps it alive, so
			// pause this feed — and only this feed.
			if errors.Is(err, domain.ErrRateLimited) {
				p.pause(target, kind, now)
			}
			continue
		}

		// Only a successful fetch advances the clock, so a failed one is retried
		// on the next tick rather than waiting out the whole interval.
		p.markFetched(target.PK, kind, now)

		// Instagram returns newest first; deliver in chronological order.
		media = append(media, reverse(fetched)...)
	}

	if len(failures) == len(due) {
		return fmt.Errorf("every due feed failed: %w", errors.Join(failures...))
	}

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

		// The first pass only establishes a baseline, otherwise starting the bot
		// would replay the entire visible history into the chat.
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
	if !initialized && len(failures) == 0 {
		err = p.repo.MarkInitialized(ctx, target)
		if err != nil {
			return err
		}
		log.Info().Str("target", target.Username).Int("baselined", fresh).Msg("baseline established, future media will be forwarded")
	}

	// A failed feed is still worth returning when Instagram is the reason: that
	// means the session or the host is blocked, not that the feed was empty.
	for _, failure := range failures {
		if isBlockedErr(failure) {
			return errors.Join(failures...)
		}
	}

	return nil
}

func (p *Poller) fetch(ctx context.Context, target domain.User, kind domain.Kind) ([]domain.Media, error) {
	if kind == domain.KindStory {
		return p.insta.Stories(ctx, target)
	}

	return p.insta.Posts(ctx, target)
}

func (p *Poller) markFetched(pk string, kind domain.Kind, now time.Time) {
	state := p.feed(pk, kind)

	state.last = now
	state.backoff = 0
	state.pausedUntil = time.Time{}
	state.blocked = nil
}

// pause backs one feed off after Instagram throttled it.
func (p *Poller) pause(target domain.User, kind domain.Kind, now time.Time) {
	state := p.feed(target.PK, kind)

	state.backoff = min(max(state.backoff*pollBackoffFactor, minBackoff), maxBackoff)
	state.pausedUntil = now.Add(state.backoff)

	log.Warn().
		Str("target", target.Username).
		Str("kind", string(kind)).
		Dur("paused_for", state.backoff).
		Msg("instagram is throttling this feed, pausing it")
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
//
// It ignores the schedule and any feed pause, because it is a manual check —
// the point is what Instagram will serve at this moment.
func (p *Poller) Probe(ctx context.Context) domain.Probe {
	start := time.Now()
	probe := domain.Probe{Targets: make([]domain.TargetProbe, 0, len(p.targets))}

	for _, target := range p.targets {
		probe.Targets = append(probe.Targets, domain.TargetProbe{
			User:    target,
			Posts:   p.probeFeed(ctx, target, domain.KindPost),
			Stories: p.probeFeed(ctx, target, domain.KindStory),
		})
	}

	probe.Elapsed = time.Since(start)

	return probe
}

func (p *Poller) probeFeed(ctx context.Context, target domain.User, kind domain.Kind) domain.FeedProbe {
	media, err := p.fetch(ctx, target, kind)
	if err != nil {
		return domain.FeedProbe{Err: err}
	}

	return domain.FeedProbe{Count: len(media), Latest: newest(media)}
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
