package logging

import (
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/arvlas/instalker/internal/metrics"
)

// errorCounterHook ties the logger to the metrics registry: every error-level
// event bumps the error counter, so metrics cannot drift from the logs.
type errorCounterHook struct{}

func (errorCounterHook) Run(_ *zerolog.Event, level zerolog.Level, _ string) {
	if level >= zerolog.ErrorLevel {
		metrics.IncErrors()
	}
}

// Init configures the global logger. It is called once, from main.
func Init(level string) {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix

	parsed, err := zerolog.ParseLevel(level)
	if err != nil {
		parsed = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(parsed)

	log.Logger = zerolog.New(os.Stdout).
		With().Timestamp().Caller().Logger().
		Hook(errorCounterHook{})
}
