package ratelimit

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// allowScript does the whole decision in one atomic round trip:
// prune expired entries, count, conditionally add, refresh TTL, and report
// when the oldest entry will leave the window. Scores are microseconds.
//
// Returns {allowed, count_after, reset_after_us}.
var allowScript = redis.NewScript(`
local key     = KEYS[1]
local now     = tonumber(ARGV[1])
local window  = tonumber(ARGV[2])
local limit   = tonumber(ARGV[3])
local member  = ARGV[4]

redis.call('ZREMRANGEBYSCORE', key, '-inf', now - window)
local count = redis.call('ZCARD', key)
local allowed = 0
if count < limit then
  redis.call('ZADD', key, now, member)
  count = count + 1
  allowed = 1
end
-- keep the key alive slightly longer than the window so stragglers are pruned lazily
redis.call('PEXPIRE', key, math.ceil(window / 1000) + 1000)

local reset = 0
local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
if oldest[2] then
  reset = tonumber(oldest[2]) + window - now
  if reset < 0 then reset = 0 end
end
return {allowed, count, reset}
`)

// RedisLimiter is a sliding-window-log limiter backed by a Redis sorted set
// per key. Safe for concurrent use and for many processes sharing one Redis.
type RedisLimiter struct {
	rdb redis.Scripter
	cfg Config
	now func() time.Time
}

// NewRedis returns a limiter that stores state in Redis. rdb may be a
// *redis.Client or any client implementing redis.Scripter (cluster, ring).
func NewRedis(rdb redis.Scripter, cfg Config) (*RedisLimiter, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &RedisLimiter{rdb: rdb, cfg: cfg, now: time.Now}, nil
}

// Allow records the request for key if it is within the limit and reports
// the decision. One Redis round trip.
func (l *RedisLimiter) Allow(ctx context.Context, key string) (Result, error) {
	nowUS := l.now().UnixMicro()
	windowUS := l.cfg.Window.Microseconds()

	vals, err := allowScript.Run(ctx, l.rdb,
		[]string{l.cfg.KeyPrefix + key},
		nowUS, windowUS, l.cfg.Limit, uniqueMember(nowUS),
	).Int64Slice()
	if err != nil {
		return Result{}, fmt.Errorf("ratelimit: redis: %w", err)
	}
	if len(vals) != 3 {
		return Result{}, fmt.Errorf("ratelimit: redis: unexpected script reply %v", vals)
	}

	res := Result{
		Allowed:    vals[0] == 1,
		Limit:      l.cfg.Limit,
		Remaining:  max(l.cfg.Limit-vals[1], 0),
		ResetAfter: time.Duration(vals[2]) * time.Microsecond,
	}
	if !res.Allowed {
		res.RetryAfter = res.ResetAfter
	}
	return res, nil
}

// uniqueMember builds a sorted-set member that cannot collide for two
// requests in the same microsecond: timestamp plus 64 random bits.
func uniqueMember(nowUS int64) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%d-%x", nowUS, binary.LittleEndian.Uint64(b[:]))
}
