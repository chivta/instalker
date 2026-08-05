package config

import (
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/go-playground/validator/v10"
	"github.com/joho/godotenv"
)

// Config is the fully validated runtime configuration of the service.
type Config struct {
	// Instagram credentials. Password login is only attempted when SessionID is empty.
	Username  string `env:"USERNAME"     validate:"required"`
	Password  string `env:"PASSWORD"     validate:"required"`
	SessionID string `env:"IG_SESSIONID"`

	// Telegram delivery.
	BotToken string `env:"BOT_TOKEN" validate:"required"`
	ChatID   int64  `env:"CHAT_ID"   validate:"required"`

	// Targets pins the watched accounts. When empty they are resolved from the
	// accounts the logged-in user follows.
	Targets []string `env:"TARGETS" envSeparator:","`

	PollInterval time.Duration `env:"POLL_INTERVAL" validate:"required,min=1m"`
	DBPath       string        `env:"DB_PATH"       validate:"required"`
	HTTPAddr     string        `env:"HTTP_ADDR"     validate:"required"`
	LogLevel     string        `env:"LOG_LEVEL"     validate:"required,oneof=debug info warn error"`
}

// Load reads .env when present, overlays the process environment and validates
// the result. Any problem is fatal for the caller.
func Load() (Config, error) {
	_ = godotenv.Load()

	cfg := Config{
		PollInterval: 5 * time.Minute,
		DBPath:       "data/instalker.db",
		HTTPAddr:     ":8080",
		LogLevel:     "info",
	}

	err := env.Parse(&cfg)
	if err != nil {
		return Config{}, fmt.Errorf("parse env: %w", err)
	}

	err = validator.New().Struct(cfg)
	if err != nil {
		return Config{}, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}
