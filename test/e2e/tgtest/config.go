// Package tgtest drives a Telegram bot the way a person does: from a real user
// account over MTProto.
//
// Unit tests cover handlers in isolation. This covers what only breaks in the
// real thing — whether the bot is reachable at all, whether a command is
// routed, and what the chat actually ends up showing.
package tgtest

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gotd/td/session"
)

// Credentials are shared across projects: one spare account and one staging bot
// serve every bot under test, so they live in the home directory rather than in
// any single repository.
//
// Search order lets a project override without touching the shared setup.
const (
	// EnvVar names a credentials file explicitly.
	EnvVar = "TGTEST_ENV"
	// LocalFile is the per-project override, if present.
	LocalFile = ".env.e2e"
	// SharedDir is where the shared credentials and session live.
	SharedDir = ".config/tgtest"
	// SharedFile is the credentials file inside SharedDir.
	SharedFile = "env"
	// SessionFileName is the authorised MTProto session inside SharedDir.
	SessionFileName = "session.json"
)

// Config is everything the tests need that cannot be derived.
type Config struct {
	// APIID and APIHash come from https://my.telegram.org/apps and are what
	// make a user (rather than bot) client possible at all.
	APIID   int
	APIHash string

	// Phone is only used by the login command, the first time.
	Phone string
	// SessionFile holds the authorised session once login has run.
	SessionFile string

	// BotToken is the bot under test. It must NOT be a production token: two
	// clients polling one token fight over updates, and the loser silently
	// never sees them.
	BotToken string
}

// LoadConfig reads credentials from the first file that exists, then overlays
// the process environment.
func LoadConfig() (Config, error) {
	values := map[string]string{}

	path, err := configPath()
	if err != nil {
		return Config{}, err
	}
	if path != "" {
		values, err = readEnvFile(path)
		if err != nil {
			return Config{}, err
		}
	}

	// The real environment wins, so a single test run can point at another bot.
	for _, key := range []string{"TG_API_ID", "TG_API_HASH", "TG_PHONE", "TG_SESSION_FILE", "TG_BOT_TOKEN"} {
		value, ok := os.LookupEnv(key)
		if ok {
			values[key] = value
		}
	}

	cfg := Config{
		APIHash:  values["TG_API_HASH"],
		Phone:    values["TG_PHONE"],
		BotToken: values["TG_BOT_TOKEN"],
	}

	if values["TG_API_ID"] != "" {
		cfg.APIID, err = strconv.Atoi(values["TG_API_ID"])
		if err != nil {
			return Config{}, fmt.Errorf("TG_API_ID is not a number: %w", err)
		}
	}

	cfg.SessionFile = values["TG_SESSION_FILE"]
	if cfg.SessionFile == "" {
		cfg.SessionFile, err = defaultSessionFile()
		if err != nil {
			return Config{}, err
		}
	}

	err = cfg.validate(path)
	if err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (c Config) validate(source string) error {
	var missing []string

	if c.APIID == 0 {
		missing = append(missing, "TG_API_ID")
	}
	if c.APIHash == "" {
		missing = append(missing, "TG_API_HASH")
	}
	if c.BotToken == "" {
		missing = append(missing, "TG_BOT_TOKEN")
	}

	if len(missing) > 0 {
		where := source
		if where == "" {
			where = "no credentials file found"
		}
		return fmt.Errorf("missing %s (%s)", strings.Join(missing, ", "), where)
	}

	return nil
}

// SessionStorage is where the authorised session is kept between runs.
func (c Config) SessionStorage() *session.FileStorage {
	return &session.FileStorage{Path: c.SessionFile}
}

// configPath returns the credentials file to read, or "" when none exists.
func configPath() (string, error) {
	explicit, ok := os.LookupEnv(EnvVar)
	if ok && explicit != "" {
		return explicit, nil
	}

	_, err := os.Stat(LocalFile)
	if err == nil {
		return LocalFile, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home directory: %w", err)
	}

	shared := filepath.Join(home, SharedDir, SharedFile)
	_, err = os.Stat(shared)
	if err == nil {
		return shared, nil
	}

	return "", nil
}

func defaultSessionFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home directory: %w", err)
	}

	return filepath.Join(home, SharedDir, SessionFileName), nil
}

// readEnvFile parses KEY=VALUE lines, ignoring blanks and # comments. Keeping
// this dependency-free is deliberate: the harness should drop into any project
// without dragging a config library with it.
func readEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	values := map[string]string{}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}

		values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}

	err = scanner.Err()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return values, nil
}
