package main

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RateLimiter implements a sliding window rate limiter using Redis
type RateLimiter struct {
	redis            *redis.Client
	windowSize       time.Duration
	maxRequests      int64
	cleanupThreshold int64
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(redisClient *redis.Client, windowSize time.Duration, maxRequests int64) *RateLimiter {
	return &RateLimiter{
		redis:            redisClient,
		windowSize:       windowSize,
		maxRequests:      maxRequests,
		cleanupThreshold: int64(windowSize.Seconds()),
	}
}

// Allow checks if a request should be allowed for the given identifier
func (rl *RateLimiter) Allow(ctx context.Context, identifier string) (bool, error) {
	now := time.Now()
	windowStart := now.Add(-rl.windowSize).Unix()
	currentTime := now.Unix()

	key := fmt.Sprintf("rate_limit:%s", identifier)

	// Use Lua script for atomic operations
	script := redis.NewScript(`
		local key = KEYS[1]
		local now = tonumber(ARGV[1])
		local window_start = tonumber(ARGV[2])
		local max_requests = tonumber(ARGV[3])

		-- Remove old entries outside the window
		redis.call('ZREMRANGEBYSCORE', key, '-inf', window_start)

		-- Count requests in current window
		local current_count = redis.call('ZCARD', key)

		if current_count < max_requests then
			-- Add current request
			redis.call('ZADD', key, now, now .. '-' .. math.random())
			-- Set expiry
			redis.call('EXPIRE', key, 3600)
			return 1
		end

		return 0
	`)

	result, err := script.Run(ctx, rl.redis, []string{key}, currentTime, windowStart, rl.maxRequests).Result()
	if err != nil {
		return false, fmt.Errorf("rate limiter script error: %w", err)
	}

	allowed := result.(int64) == 1
	return allowed, nil
}

// GetCurrentLoad returns the current number of requests in the window for an identifier
func (rl *RateLimiter) GetCurrentLoad(ctx context.Context, identifier string) (int64, error) {
	key := fmt.Sprintf("rate_limit:%s", identifier)
	windowStart := time.Now().Add(-rl.windowSize).Unix()

	// Remove expired entries
	rl.redis.ZRemRangeByScore(ctx, key, "-inf", fmt.Sprintf("%d", windowStart))

	count, err := rl.redis.ZCard(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("get current load error: %w", err)
	}

	return count, nil
}

// Reset resets the rate limit for a given identifier
func (rl *RateLimiter) Reset(ctx context.Context, identifier string) error {
	key := fmt.Sprintf("rate_limit:%s", identifier)
	return rl.redis.Del(ctx, key).Err()
}

// Stats returns rate limit statistics for an identifier
type Stats struct {
	CurrentLoad    int64
	MaxRequests    int64
	WindowSize     time.Duration
	AvailableSlots int64
	UsagePercent   float64
}

// GetStats returns current statistics for an identifier
func (rl *RateLimiter) GetStats(ctx context.Context, identifier string) (Stats, error) {
	currentLoad, err := rl.GetCurrentLoad(ctx, identifier)
	if err != nil {
		return Stats{}, err
	}

	availableSlots := rl.maxRequests - currentLoad
	usagePercent := (float64(currentLoad) / float64(rl.maxRequests)) * 100

	return Stats{
		CurrentLoad:    currentLoad,
		MaxRequests:    rl.maxRequests,
		WindowSize:     rl.windowSize,
		AvailableSlots: availableSlots,
		UsagePercent:   usagePercent,
	}, nil
}
