//go:build e2e

package e2e

import (
	"database/sql"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/arvlas/instalker/test/e2e/tgtest"
)

// startBot launches instalker with no Instagram credentials, so scraping fails
// and it stalls before polling — the state these tests exercise. It returns the
// database path so assertions can check what the bot actually persisted rather
// than trusting a reply that claims success.
func startBot(t *testing.T, cfg tgtest.Config, chatID int64) (*tgtest.Process, string) {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "e2e.db")

	bot, err := tgtest.Start(buildBinary(t), map[string]string{
		"USERNAME":     "e2e-no-instagram",
		"PASSWORD":     "e2e-no-instagram",
		"IG_SESSIONID": "",
		"BOT_TOKEN":    cfg.BotToken,
		"CHAT_ID":      fmt.Sprintf("%d", chatID),
		"TARGETS":      "",
		"DB_PATH":      dbPath,
		// Port 0 keeps concurrent runs from fighting over the probe port.
		"HTTP_ADDR":     "127.0.0.1:0",
		"POLL_INTERVAL": "1m",
		"LOG_LEVEL":     "info",
	})
	if err != nil {
		t.Fatalf("start bot: %v", err)
	}

	return bot, dbPath
}

// storedSession reads the session the bot persisted, which is the difference
// between a reply that claims success and a rotation that actually happened.
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
		t.Fatalf("read stored session: %v", err)
	}

	return value
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
