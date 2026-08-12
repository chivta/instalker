//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/arvlas/instalker/test/e2e/tgtest"
)

// logPoll is how often the bot's output is re-read while waiting for a line.
const logPoll = 200 * time.Millisecond

// botProcess is the bot under test, running as it does in production: a real
// process with its own database, talking to the staging bot token.
type botProcess struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	dbPath string

	mu  sync.Mutex
	out strings.Builder
}

// startBot launches the bot with no Instagram credentials, so scraping fails
// and it stalls before polling — the state these tests exercise.
func startBot(t *testing.T, cfg tgtest.Config, chatID int64) *botProcess {
	t.Helper()

	binary := buildBinary(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "e2e.db")

	ctx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, binary)
	cmd.Env = append(cmd.Environ(),
		"USERNAME=e2e-no-instagram",
		"PASSWORD=e2e-no-instagram",
		"IG_SESSIONID=",
		"BOT_TOKEN="+cfg.StagingBotToken,
		fmt.Sprintf("CHAT_ID=%d", chatID),
		"TARGETS=",
		"DB_PATH="+dbPath,
		// Port 0 keeps concurrent runs from fighting over the probe port.
		"HTTP_ADDR=127.0.0.1:0",
		"POLL_INTERVAL=1m",
		"LOG_LEVEL=info",
	)

	bot := &botProcess{cmd: cmd, cancel: cancel, dbPath: dbPath}
	cmd.Stdout = bot
	cmd.Stderr = bot

	err := cmd.Start()
	if err != nil {
		cancel()
		t.Fatalf("start bot: %v", err)
	}

	return bot
}

// Write collects the bot's output; it is called from the process's io goroutines.
func (b *botProcess) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.out.Write(p)
}

func (b *botProcess) logs() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.out.String()
}

// waitForLog blocks until the bot logs a line containing want.
func (b *botProcess) waitForLog(t *testing.T, want string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		if strings.Contains(b.logs(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bot never logged %q within %s\n--- output ---\n%s", want, timeout, b.logs())
		}
		time.Sleep(logPoll)
	}
}

// storedSession reads the session the bot persisted, which is the difference
// between a reply that claims success and a rotation that actually happened.
func (b *botProcess) storedSession(t *testing.T) string {
	t.Helper()

	db, err := sql.Open("sqlite", b.dbPath)
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

func (b *botProcess) stop() {
	b.cancel()
	_ = b.cmd.Wait()
}
