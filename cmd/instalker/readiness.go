package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/arvlas/instalker/internal/domain"
	"github.com/arvlas/instalker/internal/poller"
)

// errStartingUp is reported by /ping before the first startup attempt has
// produced either a poller or a failure.
var errStartingUp = errors.New("still starting up")

// readiness hands the command handlers something to answer with before polling
// has started.
//
// Commands are served from the moment the process comes up, which is deliberate:
// startup can spend minutes retrying a throttled Instagram, and that is exactly
// when being able to ask what is wrong — or to paste a fresh session — matters
// most. Until the poller exists, /ping reports why.
type readiness struct {
	mu      sync.RWMutex
	watcher *poller.Poller
	err     error
}

// polling records that startup succeeded and probes can run for real.
func (r *readiness) polling(watcher *poller.Poller) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.watcher = watcher
	r.err = nil
}

// stalled records why startup has not finished yet.
func (r *readiness) stalled(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.err = err
}

// probe runs a scrape check, or explains why one cannot run yet.
func (r *readiness) probe(ctx context.Context) (domain.Probe, error) {
	r.mu.RLock()
	watcher, err := r.watcher, r.err
	r.mu.RUnlock()

	if watcher == nil {
		if err == nil {
			err = errStartingUp
		}
		// The reply already says polling has not started; this only has to
		// carry the reason.
		return domain.Probe{}, fmt.Errorf("startup: %w", err)
	}

	return watcher.Probe(ctx), nil
}
