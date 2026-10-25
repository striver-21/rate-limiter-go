package ratelimit

import (
	"context"
	"sync"
	"time"
)

// MemoryLimiter is a sliding-window-log limiter held in process memory.
// Use it for single-instance services, local development and tests; it does
// not share state between processes.
type MemoryLimiter struct {
	cfg Config
	now func() time.Time

	mu   sync.Mutex
	logs map[string][]time.Time // per key, ascending timestamps within the window
}

// NewMemory returns an in-memory limiter.
func NewMemory(cfg Config) (*MemoryLimiter, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &MemoryLimiter{cfg: cfg, now: time.Now, logs: make(map[string][]time.Time)}, nil
}

// Allow records the request for key if it is within the limit and reports
// the decision.
func (l *MemoryLimiter) Allow(_ context.Context, key string) (Result, error) {
	now := l.now()
	cutoff := now.Add(-l.cfg.Window)

	l.mu.Lock()
	defer l.mu.Unlock()

	log := l.logs[key]
	// drop entries that fell out of the window (they are sorted ascending)
	i := 0
	for i < len(log) && !log[i].After(cutoff) {
		i++
	}
	log = log[i:]

	res := Result{Limit: l.cfg.Limit}
	if int64(len(log)) < l.cfg.Limit {
		log = append(log, now)
		res.Allowed = true
	}
	res.Remaining = max(l.cfg.Limit-int64(len(log)), 0)
	if len(log) > 0 {
		res.ResetAfter = log[0].Add(l.cfg.Window).Sub(now)
	}
	if !res.Allowed {
		res.RetryAfter = res.ResetAfter
	}

	if len(log) == 0 {
		delete(l.logs, key)
	} else {
		l.logs[key] = log
	}
	return res, nil
}

// Len reports how many keys currently hold state. Useful in tests and for
// exposing a gauge.
func (l *MemoryLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.logs)
}
