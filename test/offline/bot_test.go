// Package offline drives the bot against a fake Bot API server.
//
// No network, no credentials, no Telegram: it runs anywhere, including CI, and
// finishes in seconds. The bot itself is untouched — a real process, real HTTP
// client, real polling loop — so the wiring these tests cover is the real
// wiring.
package offline

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/arvlas/instalker/internal/tgfake"
)

const (
	chatID = int64(4242)
	// replyWait is generous relative to a local round trip, so a slow CI
	// machine does not produce a flake.
	replyWait = 30 * time.Second
	// silenceWait is how long a command that must be ignored is watched.
	silenceWait = 3 * time.Second

	// sessionPrefix builds a value that passes the bot's format check but is
	// not a real cookie.
	sessionPrefix = "10000000001%3AAaAaAaAaAaAaAa%3A25%3AFake"
)

func TestPingBeforePollingStarts(t *testing.T) {
	fake, _ := startBot(t, chatID)

	fake.SendCommand(chatID, "/ping")

	reply, err := fake.WaitForMessage("Not polling yet", replyWait)
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Logf("reply: %s", reply)
}

func TestSessionCommand(t *testing.T) {
	t.Run("without a payload it explains itself", func(t *testing.T) {
		fake, _ := startBot(t, chatID)

		fake.SendCommand(chatID, "/session")

		_, err := fake.WaitForMessage("Send the cookie with the command", replyWait)
		if err != nil {
			t.Fatalf("session usage: %v", err)
		}
	})

	t.Run("a malformed value is rejected", func(t *testing.T) {
		fake, dbPath := startBot(t, chatID)

		fake.SendCommand(chatID, "/session obviously-not-a-cookie")

		_, err := fake.WaitForMessage("does not look like a session cookie", replyWait)
		if err != nil {
			t.Fatalf("malformed session: %v", err)
		}

		// A typo must not reach the database.
		if stored := storedSession(t, dbPath); stored != "" {
			t.Errorf("stored %q, want nothing persisted", stored)
		}
	})

	t.Run("a well-formed value is applied and persisted", func(t *testing.T) {
		fake, dbPath := startBot(t, chatID)

		session := fmt.Sprintf("%s%d", sessionPrefix, time.Now().UnixNano())
		fake.SendCommand(chatID, "/session "+session)

		_, err := fake.WaitForMessage("Session updated and saved", replyWait)
		if err != nil {
			t.Fatalf("session update: %v", err)
		}

		// The reply claiming success and the row existing are different claims.
		if stored := waitForStoredSession(t, dbPath, replyWait); stored != session {
			t.Errorf("stored %q, want the session that was sent", stored)
		}
	})

	t.Run("the message carrying the cookie is deleted", func(t *testing.T) {
		fake, _ := startBot(t, chatID)

		fake.SendCommand(chatID, "/session "+sessionPrefix+"1")

		_, err := fake.WaitForCall("deleteMessage", "", "", replyWait)
		if err != nil {
			t.Fatalf("the credential message was not deleted: %v", err)
		}
	})
}

// TestIgnoresOtherChats is the one that matters most: a bot's username is
// public, so chat gating is often all that stands between a stranger and a
// credential-setting command.
func TestIgnoresOtherChats(t *testing.T) {
	fake, dbPath := startBot(t, chatID)

	foreign := chatID + 1
	fake.SendCommand(foreign, "/session "+sessionPrefix+"2")

	// Scoped to the foreign chat: the bot legitimately messages its own chat
	// about unrelated things, and counting those would be a false alarm.
	if reply := fake.ExpectNoMessageTo(foreign, silenceWait); reply != "" {
		t.Errorf("bot answered a chat it is not configured for: %q", reply)
	}
	if stored := storedSession(t, dbPath); stored != "" {
		t.Errorf("a foreign chat set the session to %q", stored)
	}
}

// TestSurvivesTelegramErrors checks the bot keeps serving after the API fails
// under it. Provoking this is the thing a fake can do that the real API cannot.
func TestSurvivesTelegramErrors(t *testing.T) {
	fake, _ := startBot(t, chatID)

	fake.FailNext("sendMessage", 400, "Bad Request: message is too long")
	fake.SendCommand(chatID, "/ping")

	// The failure is swallowed; the next command must still be answered.
	time.Sleep(500 * time.Millisecond)
	fake.SendCommand(chatID, "/ping")

	_, err := fake.WaitForMessage("Not polling yet", replyWait)
	if err != nil {
		t.Fatalf("bot stopped answering after a send failure: %v", err)
	}
}

// startBot runs instalker against a fake API with no Instagram credentials, so
// scraping fails and it stalls before polling — the state where commands matter
// most. It returns the fake and the bot's database path.
func startBot(t *testing.T, chat int64) (*tgfake.Server, string) {
	t.Helper()

	fake := tgfake.New()
	t.Cleanup(fake.Close)

	dbPath := filepath.Join(t.TempDir(), "offline.db")

	bot, err := tgfake.StartProcess(buildBinary(t), map[string]string{
		"USERNAME":     "offline-no-instagram",
		"PASSWORD":     "offline-no-instagram",
		"IG_SESSIONID": "",
		"BOT_TOKEN":    "123456:FAKE",
		"CHAT_ID":      fmt.Sprintf("%d", chat),
		"TARGETS":      "",
		"DB_PATH":      dbPath,
		// The bot must reach the fake instead of Telegram.
		"TELEGRAM_API_URL": fake.URL(),
		"HTTP_ADDR":        "127.0.0.1:0",
		"POLL_INTERVAL":    "1m",
		"LOG_LEVEL":        "info",
	})
	if err != nil {
		t.Fatalf("start bot: %v", err)
	}
	t.Cleanup(bot.Stop)

	err = bot.WaitForLog("telegram command listener started", replyWait)
	if err != nil {
		t.Fatalf("startup: %v", err)
	}

	// The bot's own log distinguishes "tried and failed" from "never tried",
	// which the API side alone cannot.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("--- bot log ---\n%s", bot.Logs())
		}
	})

	return fake, dbPath
}

// binaryPath is built once for the whole package.
//
// It deliberately does not use t.TempDir(): that directory is removed when the
// test that created it ends, which would delete the binary out from under every
// later test.
var binaryPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "instalker-offline")
	if err != nil {
		fmt.Fprintf(os.Stderr, "temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	binaryPath = filepath.Join(dir, "instalker")

	build := exec.Command("go", "build", "-o", binaryPath, "../../cmd/instalker")
	out, err := build.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func buildBinary(t *testing.T) string {
	t.Helper()

	return binaryPath
}

func storedSession(t *testing.T, dbPath string) string {
	t.Helper()

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open bot database: %v", err)
	}
	defer db.Close()

	var value string
	err = db.QueryRow(`SELECT value FROM app_state WHERE key = 'ig_session_id'`).Scan(&value)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") || strings.Contains(err.Error(), "no such table") {
			return ""
		}
		t.Fatalf("read stored session: %v", err)
	}

	return value
}

// waitForStoredSession allows for the reply arriving before the write lands.
func waitForStoredSession(t *testing.T, dbPath string, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		value := storedSession(t, dbPath)
		if value != "" || time.Now().After(deadline) {
			return value
		}
		time.Sleep(50 * time.Millisecond)
	}
}
