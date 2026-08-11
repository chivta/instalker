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
	"github.com/arvlas/instalker/internal/storage"
)

const (
	// authRetryAttempts and authRetryDelay bound the startup retry of a
	// transient Instagram failure; the delay doubles between attempts.
	authRetryAttempts = 4
	authRetryDelay    = 30 * time.Second
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

	insta, me, err := authenticate(ctx, cfg)
	if err != nil {
		// A rejected session is just as fatal as a challenge, and just as
		// invisible from the chat unless it is reported there.
		if errors.Is(err, domain.ErrCheckpointRequired) || errors.Is(err, domain.ErrUnauthorized) {
			notifyLoginFailure(ctx, telegram, cfg, err)
		}
		return err
	}
	log.Info().Str("username", me.Username).Str("pk", me.PK).Msg("instagram session ready")

	// This is the first call that actually exercises the session, so it is the
	// one that gets the retry budget.
	var targets []domain.User
	err = withBackoff(ctx, "resolve targets", func() error {
		var resolveErr error
		targets, resolveErr = poller.ResolveTargets(ctx, insta, me, cfg.Targets)
		return resolveErr
	})
	if err != nil {
		if errors.Is(err, domain.ErrCheckpointRequired) || errors.Is(err, domain.ErrUnauthorized) {
			notifyLoginFailure(ctx, telegram, cfg, err)
		}
		return err
	}
	for _, t := range targets {
		log.Info().Str("username", t.Username).Str("pk", t.PK).Bool("private", t.IsPrivate).Msg("watching target")
	}

	repo := storage.NewMediaRepo(db)
	watcher := poller.New(insta, repo, telegram, targets, cfg.PollInterval)

	err = telegram.Notify(ctx, fmt.Sprintf("🟢 instalker is up, watching <b>%s</b> every %s", strings.Join(usernames(targets), "</b>, <b>"), cfg.PollInterval))
	if err != nil {
		log.Error().Err(err).Msg("failed to send startup notice")
	}

	err = watcher.Run(ctx)

	log.Info().Msg("shutdown complete")

	return err
}

// authenticate prefers a supplied session cookie and only falls back to a
// password login when none is configured.
func authenticate(ctx context.Context, cfg config.Config) (*instagram.Client, domain.User, error) {
	insta, err := instagram.New(cfg.SessionID)
	if err != nil {
		return nil, domain.User{}, err
	}

	// A password login is only attempted when there is no session to use. It is
	// never a fallback for a session that looks rejected: Instagram answers it
	// with a challenge, which is strictly worse than retrying.
	if cfg.SessionID == "" {
		err = insta.Login(ctx, cfg.Username, cfg.Password)
		if err != nil {
			return nil, domain.User{}, err
		}
	}

	me, err := insta.SessionUser()
	if err != nil {
		return nil, domain.User{}, err
	}
	me.Username = cfg.Username

	return insta, me, nil
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
			"Log in to instagram.com in a browser, clear any challenge, then copy the <code>sessionid</code> cookie into <code>IG_SESSIONID</code> and restart.\n\n<code>%s</code>",
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
