package domain

import "errors"

// Sentinel errors describing the conditions instalker knows how to react to.
var (
	// ErrNotFound is returned when a requested Instagram entity does not exist.
	ErrNotFound = errors.New("not_found")
	// ErrUnauthorized is returned when the Instagram session is missing, expired or rejected.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrCheckpointRequired is returned when Instagram demands a manual login challenge.
	ErrCheckpointRequired = errors.New("checkpoint_required")
	// ErrRateLimited is returned when Instagram throttles the client.
	ErrRateLimited = errors.New("rate_limited")
	// ErrBadResponse is returned when Instagram answers with something unparseable.
	ErrBadResponse = errors.New("bad_response")
	// ErrTargetsUnresolved is returned when the set of accounts to watch cannot be determined.
	ErrTargetsUnresolved = errors.New("targets_unresolved")
)
