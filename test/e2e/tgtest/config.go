// Package tgtest drives the bot the way a person does: from a real Telegram
// user account over MTProto.
//
// Unit tests cover the pieces in isolation; this covers the part that only
// breaks in the real thing — whether the bot is reachable at all, whether a
// command is routed, and what the chat actually shows.
package tgtest

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/caarlos0/env/v11"
	"github.com/go-playground/validator/v10"
	"github.com/gotd/td/session"
	"github.com/joho/godotenv"
)

// ConfigFile holds the credentials for the staging bot and spare account. It is
// gitignored and never goes near CI.
const ConfigFile = ".env.e2e"

// Config is everything the end-to-end tests need that cannot be derived.
type Config struct {
	// APIID and APIHash come from https://my.telegram.org/apps and are what
	// make a user (rather than bot) client possible at all.
	APIID   int    `env:"TG_API_ID"   validate:"required"`
	APIHash string `env:"TG_API_HASH" validate:"required"`

	// Phone is only used the first time, by the login command.
	Phone string `env:"TG_PHONE"`
	// SessionFile is written by the login command and reused afterwards.
	SessionFile string `env:"TG_SESSION_FILE" validate:"required"`
	// UserID is the spare account, which is also the chat the bot replies in.
	UserID int64 `env:"TG_USER_ID"`

	// StagingBotToken must be a different bot from production: two clients
	// polling one token fight over updates, and whichever wins swallows them.
	StagingBotToken string `env:"STAGING_BOT_TOKEN" validate:"required"`
}

// LoadConfig reads .env.e2e, overlays the environment and validates the result.
func LoadConfig() (Config, error) {
	root, err := repoRoot()
	if err != nil {
		return Config{}, err
	}

	_ = godotenv.Load(filepath.Join(root, ConfigFile))

	cfg := Config{SessionFile: filepath.Join(root, ".e2e-session.json")}

	err = env.Parse(&cfg)
	if err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", ConfigFile, err)
	}

	err = validator.New().Struct(cfg)
	if err != nil {
		return Config{}, fmt.Errorf("%s is incomplete: %w", ConfigFile, err)
	}

	return cfg, nil
}

// SessionStorage is where the authenticated session is kept between runs.
func (c Config) SessionStorage() *session.FileStorage {
	return &session.FileStorage{Path: c.SessionFile}
}

// ChatID is the chat the bot under test is configured to talk to.
func (c Config) ChatID() string {
	return strconv.FormatInt(c.UserID, 10)
}

// repoRoot walks up from the working directory to the module root, so the tests
// find the config file regardless of which package they run from.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}

	for {
		_, err := os.Stat(filepath.Join(dir, "go.mod"))
		if err == nil {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found above the working directory")
		}
		dir = parent
	}
}
