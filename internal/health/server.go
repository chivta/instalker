package health

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
)

// shutdownTimeout bounds how long in-flight probe requests may drain.
const shutdownTimeout = 5 * time.Second

// Server exposes the liveness and metrics endpoints every service is expected
// to provide.
type Server struct {
	http *http.Server
}

// New builds the probe server bound to addr.
func New(addr string, metrics http.Handler) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("GET /metrics", metrics)

	return &Server{http: &http.Server{Addr: addr, Handler: mux}}
}

// Run serves until ctx is cancelled, then drains gracefully.
func (s *Server) Run(ctx context.Context) error {
	errs := make(chan error, 1)

	go func() {
		log.Info().Str("addr", s.http.Addr).Msg("probe server listening")
		err := s.http.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			errs <- fmt.Errorf("probe server: %w", err)
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		err := s.http.Shutdown(shutdownCtx)
		if err != nil {
			return fmt.Errorf("shutdown probe server: %w", err)
		}

		log.Info().Msg("probe server stopped")
		return nil
	}
}
