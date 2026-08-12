package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/arvlas/instalker/internal/config"
	"github.com/arvlas/instalker/internal/domain"
	"github.com/arvlas/instalker/internal/health"
	"github.com/arvlas/instalker/internal/instagram"
	"github.com/arvlas/instalker/internal/logging"
	"github.com/arvlas/instalker/internal/metrics"
	"github.com/arvlas/instalker/internal/notifier"
	"github.com/arvlas/instalker/internal/poller"
	"github.com/arvlas/instalker/internal/session"
	"github.com/arvlas/instalker/internal/storage"
)

const (
	// authRetryAttempts and authRetryDelay bound the startup retry of a
	// transient Instagram failure; the delay doubles between attempts.
	authRetryAttempts = 4
	authRetryDelay    = 30 * time.Second

	// startupRetryDelay is how long a startup blocked on a human-supplied
	// session waits before trying again on its own.
	startupRetryDelay = 5 * time.Minute
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	logging.Init(cfg.LogLevel)

	err = run(cfg)
	if err != nil {
		log.Error().Err(err).Msg("instalker exited with an error")
		os.Exit(1)
	}
}

func run(cfg config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := storage.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	telegram, err := notifier.NewTelegram(cfg.BotToken, cfg.ChatID)
	if err != nil {
		return err
	}

	insta, err := instagram.New("")
	if err != nil {
		return err
	}

	// The probe server comes up before Instagram is contacted. Authentication
	// can retry for minutes when Instagram is throttling, and a container with
	// nothing listening fails its liveness probe and is killed mid-retry.
	probes := health.New(cfg.HTTPAddr, metrics.Handler())
	probeErr := make(chan error, 1)
	go func() {
		probeErr <- probes.Run(ctx)
	}()
	defer func() {
		stop()
		<-probeErr
	}()

	sessions := session.New(storage.NewStateRepo(db), insta)

	// Commands are served from here on, before Instagram is contacted. Startup
	// can spend minutes retrying a throttled Instagram, and a bot that only
	// starts listening after that succeeds is silent exactly when /ping and
	// /session are needed.
	var ready readiness
	telegram.HandlePing(ctx, ready.probe)

	// A new session is what unblocks a stalled startup, so the startup loop is
	// told the moment one arrives rather than waiting out its retry delay.
	sessionChanged := make(chan struct{}, 1)
	telegram.HandleSession(ctx, func(ctx context.Context, sessionID string) error {
		err := sessions.Update(ctx, sessionID)
		if err != nil {
			return err
		}

		select {
		case sessionChanged <- struct{}{}:
		default:
		}

		return nil
	})

	commandErr := make(chan error, 1)
	go func() {
		commandErr <- telegram.Run(ctx)
	}()
	defer func() {
		stop()
		<-commandErr
	}()

	me, targets, err := awaitStartup(ctx, cfg, insta, sessions, telegram, &ready, sessionChanged)
	if err != nil {
		return err
	}
	log.Info().Str("username", me.Username).Str("pk", me.PK).Msg("instagram session ready")

	for _, t := range targets {
		log.Info().Str("username", t.Username).Str("pk", t.PK).Bool("private", t.IsPrivate).Msg("watching target")
	}

	repo := storage.NewMediaRepo(db)
	watcher := poller.New(insta, repo, telegram, targets, cfg.PollInterval)
	ready.polling(watcher)

	err = telegram.Notify(ctx, fmt.Sprintf("🟢 instalker is up, watching <b>%s</b> every %s", strings.Join(usernames(targets), "</b>, <b>"), cfg.PollInterval))
	if err != nil {
		log.Error().Err(err).Msg("failed to send startup notice")
	}

	err = watcher.Run(ctx)

	log.Info().Msg("shutdown complete")

	return err
}

// awaitStartup authenticates and resolves targets, staying alive when the only
// thing that can fix the failure is a human sending a new session.
//
// Exiting there would be self-defeating: the bot's own message tells you to
// send /session, and a dead process cannot receive it. So a rejected session or
// a pending challenge parks the bot with its commands still served, and any
// /session retries immediately.
func awaitStartup(
	ctx context.Context,
	cfg config.Config,
	insta *instagram.Client,
	sessions *session.Manager,
	telegram *notifier.Telegram,
	ready *readiness,
	sessionChanged <-chan struct{},
) (domain.User, []domain.User, error) {
	notified := false

	for {
		me, targets, err := tryStartup(ctx, cfg, insta, sessions, ready)
		if err == nil {
			return me, targets, nil
		}

		ready.stalled(err)
		log.Error().Err(err).Msg("startup failed")

		// Anything else — throttling, a bad response — is better handled by
		// exiting and letting the supervisor back off and retry.
		if !errors.Is(err, domain.ErrUnauthorized) && !errors.Is(err, domain.ErrCheckpointRequired) {
			return domain.User{}, nil, err
		}

		if !notified {
			notifyLoginFailure(ctx, telegram, cfg, err)
			notified = true
		}

		log.Warn().Dur("retry_in", startupRetryDelay).Msg("waiting for a new session before retrying startup")

		select {
		case <-ctx.Done():
			return domain.User{}, nil, ctx.Err()
		case <-sessionChanged:
			log.Info().Msg("session replaced, retrying startup")
			notified = false
		case <-time.After(startupRetryDelay):
		}
	}
}

// tryStartup is one attempt at becoming ready to poll.
func tryStartup(
	ctx context.Context,
	cfg config.Config,
	insta *instagram.Client,
	sessions *session.Manager,
	ready *readiness,
) (domain.User, []domain.User, error) {
	me, err := authenticate(ctx, cfg, insta, sessions)
	if err != nil {
		return domain.User{}, nil, err
	}

	// This is the first call that actually exercises the session, so it is the
	// one that gets the retry budget.
	var targets []domain.User
	err = withBackoff(ctx, "resolve targets", func() error {
		var resolveErr error
		targets, resolveErr = poller.ResolveTargets(ctx, insta, me, cfg.Targets)
		// Each attempt updates what /ping reports, so a session pasted during
		// the retry window is reflected on the next answer.
		ready.stalled(resolveErr)
		return resolveErr
	})
	if err != nil {
		return domain.User{}, nil, err
	}

	return me, targets, nil
}

// authenticate puts a session on the client: the stored one when there is one,
// the IG_SESSIONID bootstrap the first time, and a password login only when
// neither exists.
//
// A password login is never a fallback for a session that looks rejected;
// Instagram answers it with a challenge, which is strictly worse than retrying.
func authenticate(ctx context.Context, cfg config.Config, insta *instagram.Client, sessions *session.Manager) (domain.User, error) {
	err := sessions.Load(ctx, cfg.SessionID)
	if errors.Is(err, domain.ErrNotFound) {
		log.Info().Msg("no session stored and IG_SESSIONID is empty, attempting a password login")

		err = insta.Login(ctx, cfg.Username, cfg.Password)
		if err != nil {
			return domain.User{}, err
		}

		// Keep what the login produced so the next start does not need to log
		// in again and risk another challenge.
		storeErr := sessions.Store(ctx, insta.SessionID())
		if storeErr != nil {
			log.Error().Err(storeErr).Msg("failed to store the session from the password login")
		}
	} else if err != nil {
		return domain.User{}, err
	}

	me, err := insta.SessionUser()
	if err != nil {
		return domain.User{}, err
	}
	me.Username = cfg.Username

	return me, nil
}

// withBackoff retries an operation that failed for a transient reason.
// Instagram throttles by source address, and a pod that exits on the first 429
// is restarted straight into another request, which deepens the block.
func withBackoff(ctx context.Context, what string, op func() error) error {
	delay := authRetryDelay

	for attempt := 1; ; attempt++ {
		err := op()
		if err == nil {
			return nil
		}

		transient := errors.Is(err, domain.ErrRateLimited) || errors.Is(err, domain.ErrBadResponse)
		if !transient || attempt == authRetryAttempts {
			return err
		}

		log.Warn().Err(err).Str("operation", what).Int("attempt", attempt).Dur("retry_in", delay).Msg("instagram call failed, retrying")

		if !sleep(ctx, delay) {
			return ctx.Err()
		}
		delay *= 2
	}
}

// sleep waits for d, reporting false if the context was cancelled first.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// notifyLoginFailure reports a startup failure that only a human can clear.
// Throttling is deliberately not reported here: it is not actionable, and the
// pod restart loop would turn it into a stream of identical messages.
func notifyLoginFailure(ctx context.Context, telegram *notifier.Telegram, cfg config.Config, cause error) {
	reason := "Instagram is refusing the stored session"
	if errors.Is(cause, domain.ErrCheckpointRequired) {
		reason = "Instagram is requiring a login challenge"
	}

	text := fmt.Sprintf(
		"🔴 instalker cannot log in as <b>%s</b>: %s.\n\n"+
			"Log in to instagram.com in a browser, clear any challenge, then send the new cookie here as "+
			"<code>/session &lt;sessionid&gt;</code>.\n\n<code>%s</code>",
		cfg.Username, reason, cause,
	)

	err := telegram.Notify(ctx, text)
	if err != nil {
		log.Error().Err(err).Msg("failed to send login failure notice")
	}
}

func usernames(users []domain.User) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		out = append(out, u.Username)
	}

	return out
}
