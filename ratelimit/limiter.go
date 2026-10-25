// Package ratelimit implements a sliding-window-log rate limiter.
//
// Two implementations share one interface: Redis (for a fleet of API
// instances sharing a limit) and an in-memory one (for single-process use
// and tests). Both keep a log of request timestamps per key and allow a
// request when fewer than Limit requests happened in the trailing Window.
// This is an exact sliding window, not the fixed-bucket approximation.
package ratelimit

import (
	"context"
	"errors"
	"time"
)

// Result describes one Allow decision.
type Result struct {
	// Allowed reports whether the request may proceed.
	Allowed bool
	// Limit is the configured maximum number of requests per window.
	Limit int64
	// Remaining is how many more requests the key may make right now.
	Remaining int64
	// RetryAfter is how long until the next request would be allowed.
	// Zero when Allowed is true.
	RetryAfter time.Duration
	// ResetAfter is how long until the oldest request in the window
	// expires, i.e. until Remaining grows by at least one.
	ResetAfter time.Duration
}

// Limiter decides whether a request identified by key may proceed.
type Limiter interface {
	Allow(ctx context.Context, key string) (Result, error)
}

// Config is shared by all implementations.
type Config struct {
	// Limit is the maximum number of requests allowed per Window. Required.
	Limit int64
	// Window is the length of the sliding window. Required.
	Window time.Duration
	// KeyPrefix namespaces the keys stored in the backend. Default "ratelimit:".
	KeyPrefix string
}

// ErrInvalidConfig is returned when Limit or Window is not positive.
var ErrInvalidConfig = errors.New("ratelimit: limit and window must be > 0")

func (c *Config) validate() error {
	if c.Limit <= 0 || c.Window <= 0 {
		return ErrInvalidConfig
	}
	if c.KeyPrefix == "" {
		c.KeyPrefix = "ratelimit:"
	}
	return nil
}
