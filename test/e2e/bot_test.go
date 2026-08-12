//go:build e2e

// These tests drive the real bot from a real Telegram account. They are behind
// the e2e build tag because they need credentials and a live network:
//
//	go test -tags e2e ./test/e2e/ -v
//
// Instagram credentials are deliberately absent. Scraping then fails, which is
// the fixture: the bot reaches its command listener and stalls at target
// resolution, so everything reachable without polling is exercised.
package e2e

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arvlas/instalker/test/e2e/tgtest"
)

const (
	// replyTimeout is generous: the bot answers /ping only after its own
	// Instagram attempt times out.
	replyTimeout = 90 * time.Second
	// silenceWindow is how long a command from the wrong chat is watched.
	silenceWindow = 20 * time.Second
	// startupWindow is how long the bot gets to begin serving commands.
	startupWindow = 30 * time.Second

	// sampleSession is well-formed but not a real cookie, so it is accepted by
	// validation and rejected by Instagram.
	sampleSession = "10000000001%3AAaAaAaAaAaAaAa%3A25%3AFakeTokenValue"
)

func TestBot(t *testing.T) {
	cfg, err := tgtest.LoadConfig()
	if err != nil {
		t.Skipf("end-to-end tests need %s: %v", tgtest.ConfigFile, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	botUsername, err := tgtest.BotUsername(ctx, cfg.StagingBotToken)
	if err != nil {
		t.Fatalf("staging bot token: %v", err)
	}
	t.Logf("driving @%s", botUsername)

	err = tgtest.Run(ctx, cfg, botUsername, func(ctx context.Context, user *tgtest.User) error {
		bot := startBot(t, cfg, user.SelfID())
		defer bot.stop()

		// /start also creates the conversation, which a bot cannot do itself.
		err := user.Send(ctx, "/start")
		if err != nil {
			return err
		}
		bot.waitForLog(t, "telegram command listener started", startupWindow)

		t.Run("ping reports that polling has not started", func(t *testing.T) {
			err := user.Send(ctx, "/ping")
			if err != nil {
				t.Fatalf("send: %v", err)
			}

			reply, err := user.WaitForReply(ctx, "Not polling yet", replyTimeout)
			if err != nil {
				t.Fatalf("ping: %v", err)
			}
			t.Logf("reply: %s", reply)
		})

		t.Run("session without a payload explains itself", func(t *testing.T) {
			err := user.Send(ctx, "/session")
			if err != nil {
				t.Fatalf("send: %v", err)
			}

			_, err = user.WaitForReply(ctx, "Send the cookie with the command", replyTimeout)
			if err != nil {
				t.Fatalf("session usage: %v", err)
			}
		})

		t.Run("malformed session is rejected", func(t *testing.T) {
			err := user.Send(ctx, "/session obviously-not-a-cookie")
			if err != nil {
				t.Fatalf("send: %v", err)
			}

			_, err = user.WaitForReply(ctx, "does not look like a session cookie", replyTimeout)
			if err != nil {
				t.Fatalf("malformed session: %v", err)
			}
		})

		t.Run("well-formed session is stored and its message deleted", func(t *testing.T) {
			command := "/session " + sampleSession

			err := user.Send(ctx, command)
			if err != nil {
				t.Fatalf("send: %v", err)
			}

			_, err = user.WaitForReply(ctx, "Session updated and saved", replyTimeout)
			if err != nil {
				t.Fatalf("session update: %v", err)
			}

			// The message carried a credential and must not survive in the chat.
			stillThere, err := user.SentMessageExists(ctx, command)
			if err != nil {
				t.Fatalf("history: %v", err)
			}
			if stillThere {
				t.Error("the message carrying the session cookie was not deleted")
			}

			stored := bot.storedSession(t)
			if stored != sampleSession {
				t.Errorf("stored session = %q, want the one that was sent", stored)
			}
		})

		return nil
	})
	if err != nil {
		t.Fatalf("telegram session: %v", err)
	}
}

// TestBotIgnoresOtherChats points the bot at a different chat id and checks that
// commands from the spare account are ignored. The bot's username is public, so
// this is the only thing standing between a stranger and the /session command.
func TestBotIgnoresOtherChats(t *testing.T) {
	cfg, err := tgtest.LoadConfig()
	if err != nil {
		t.Skipf("end-to-end tests need %s: %v", tgtest.ConfigFile, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	botUsername, err := tgtest.BotUsername(ctx, cfg.StagingBotToken)
	if err != nil {
		t.Fatalf("staging bot token: %v", err)
	}

	err = tgtest.Run(ctx, cfg, botUsername, func(ctx context.Context, user *tgtest.User) error {
		// Configured for someone else entirely.
		bot := startBot(t, cfg, user.SelfID()+1)
		defer bot.stop()

		bot.waitForLog(t, "telegram command listener started", startupWindow)

		err := user.Send(ctx, "/ping")
		if err != nil {
			return err
		}

		err = user.ExpectSilence(ctx, silenceWindow)
		if err != nil {
			t.Errorf("bot answered a chat it is not configured for: %v", err)
		}

		if !strings.Contains(bot.logs(), "ignoring command from an unknown chat") {
			t.Error("the rejected command was not logged")
		}

		return nil
	})
	if err != nil {
		t.Fatalf("telegram session: %v", err)
	}
}

// buildBinary compiles the bot once per run.
func buildBinary(t *testing.T) string {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "instalker")

	build := exec.Command("go", "build", "-o", binary, "../../cmd/instalker")
	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	return binary
}
