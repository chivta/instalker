package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

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

	insta, me, err := authenticate(ctx, cfg)
	if err != nil {
		// A rejected session is just as fatal as a challenge, and just as
		// invisible from the chat unless it is reported there.
		if errors.Is(err, domain.ErrCheckpointRequired) || errors.Is(err, domain.ErrUnauthorized) {
			notifyCheckpoint(ctx, telegram, cfg, err)
		}
		return err
	}
	log.Info().Str("username", me.Username).Str("pk", me.PK).Msg("instagram session ready")

	targets, err := poller.ResolveTargets(ctx, insta, me, cfg.Targets)
	if err != nil {
		return err
	}
	for _, t := range targets {
		log.Info().Str("username", t.Username).Str("pk", t.PK).Bool("private", t.IsPrivate).Msg("watching target")
	}

	repo := storage.NewMediaRepo(db)
	watcher := poller.New(insta, repo, telegram, targets, cfg.PollInterval)
	probes := health.New(cfg.HTTPAddr, metrics.Handler())

	err = telegram.Notify(ctx, fmt.Sprintf("🟢 instalker is up, watching <b>%s</b> every %s", strings.Join(usernames(targets), "</b>, <b>"), cfg.PollInterval))
	if err != nil {
		log.Error().Err(err).Msg("failed to send startup notice")
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)

	wg.Add(2)
	go func() {
		defer wg.Done()
		errs <- watcher.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		errs <- probes.Run(ctx)
	}()

	wg.Wait()
	close(errs)

	var joined error
	for err := range errs {
		joined = errors.Join(joined, err)
	}

	log.Info().Msg("shutdown complete")

	return joined
}

// authenticate prefers a supplied session cookie and only falls back to a
// password login when none is configured.
func authenticate(ctx context.Context, cfg config.Config) (*instagram.Client, domain.User, error) {
	insta, err := instagram.New(cfg.SessionID)
	if err != nil {
		return nil, domain.User{}, err
	}

	if cfg.SessionID != "" {
		me, err := insta.VerifySession(ctx)
		if err == nil {
			return insta, me, nil
		}
		log.Warn().Err(err).Msg("configured IG_SESSIONID was rejected, falling back to password login")
	}

	err = insta.Login(ctx, cfg.Username, cfg.Password)
	if err != nil {
		return nil, domain.User{}, err
	}

	me, err := insta.VerifySession(ctx)
	if err != nil {
		return nil, domain.User{}, err
	}

	return insta, me, nil
}

func notifyCheckpoint(ctx context.Context, telegram *notifier.Telegram, cfg config.Config, cause error) {
	text := fmt.Sprintf(
		"🔴 instalker cannot log in as <b>%s</b>: Instagram is requiring a login challenge.\n\n"+
			"Log in to instagram.com in a browser, clear the challenge, then copy the <code>sessionid</code> cookie into <code>IG_SESSIONID</code> in .env and restart.\n\n<code>%s</code>",
		cfg.Username, cause,
	)

	err := telegram.Notify(ctx, text)
	if err != nil {
		log.Error().Err(err).Msg("failed to send checkpoint notice")
	}
}

func usernames(users []domain.User) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		out = append(out, u.Username)
	}

	return out
}
