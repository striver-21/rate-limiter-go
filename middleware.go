package main

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// Middleware creates a Gin middleware for rate limiting
func Middleware(limiter *RateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Use IP address or user ID as identifier
		identifier := c.ClientIP()
		if userID := c.GetString("user_id"); userID != "" {
			identifier = userID
		}

		allowed, err := limiter.Allow(c.Request.Context(), identifier)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "rate limiter error"})
			c.Abort()
			return
		}

		// Add rate limit headers
		stats, _ := limiter.GetStats(c.Request.Context(), identifier)
		c.Header("X-RateLimit-Limit", strconv.FormatInt(stats.MaxRequests, 10))
		c.Header("X-RateLimit-Remaining", strconv.FormatInt(stats.AvailableSlots, 10))
		c.Header("X-RateLimit-Used", strconv.FormatInt(stats.CurrentLoad, 10))

		if !allowed {
			c.Header("Retry-After", "1")
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":   "rate limit exceeded",
				"message": fmt.Sprintf("Max %d requests per %v", stats.MaxRequests, stats.WindowSize),
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

// StatsHandler returns rate limit statistics for the current client
func StatsHandler(limiter *RateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		identifier := c.ClientIP()
		if userID := c.GetString("user_id"); userID != "" {
			identifier = userID
		}

		stats, err := limiter.GetStats(c.Request.Context(), identifier)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"identifier":      identifier,
			"current_load":    stats.CurrentLoad,
			"max_requests":    stats.MaxRequests,
			"available_slots": stats.AvailableSlots,
			"usage_percent":   fmt.Sprintf("%.2f%%", stats.UsagePercent),
			"window_size":     stats.WindowSize.String(),
		})
	}
}

// ResetHandler resets rate limit for testing
func ResetHandler(limiter *RateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		identifier := c.Query("identifier")
		if identifier == "" {
			identifier = c.ClientIP()
		}

		if err := limiter.Reset(c.Request.Context(), identifier); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"message": fmt.Sprintf("Rate limit reset for %s", identifier),
		})
	}
}

// HealthCheckHandler is a health check endpoint
func HealthCheckHandler(redisClient *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := redisClient.Ping(c.Request.Context()).Err(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unhealthy", "error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"status": "healthy"})
	}
}
