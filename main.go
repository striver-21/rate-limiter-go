package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

func main() {
	// Initialize Redis client
	redisClient := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
	defer redisClient.Close()

	// Test Redis connection
	ctx := context.Background()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	log.Println("Connected to Redis")

	// Create rate limiters with different configurations
	// 5000 requests per second (sliding window)
	limiter := NewRateLimiter(redisClient, time.Second, 5000)

	// Initialize Gin router
	router := gin.Default()

	// Health check
	router.GET("/health", HealthCheckHandler(redisClient))

	// Rate limit stats endpoint (no limiting)
	router.GET("/rate-limit/stats", StatsHandler(limiter))
	router.POST("/rate-limit/reset", ResetHandler(limiter))

	// Protected routes with rate limiting
	api := router.Group("/api")
	api.Use(Middleware(limiter))
	{
		api.GET("/data", handleGetData)
		api.POST("/data", handlePostData)
		api.GET("/users/:id", handleGetUser)
	}

	// Demo endpoint for testing
	router.POST("/demo/burst", func(c *gin.Context) {
		identifier := "burst-test"
		stats, _ := limiter.GetStats(c.Request.Context(), identifier)

		c.JSON(http.StatusOK, gin.H{
			"message": "Burst requests processed",
			"stats": gin.H{
				"current_load":    stats.CurrentLoad,
				"max_requests":    stats.MaxRequests,
				"available_slots": stats.AvailableSlots,
				"usage_percent":   fmt.Sprintf("%.2f%%", stats.UsagePercent),
			},
		})
	})

	// Start server
	port := ":8080"
	fmt.Printf("Starting rate limiter server on %s\n", port)
	fmt.Printf("Configuration: 5,000 requests per second\n")
	fmt.Printf("\nEndpoints:\n")
	fmt.Printf("  GET    /health              - Health check\n")
	fmt.Printf("  GET    /rate-limit/stats    - Current rate limit stats\n")
	fmt.Printf("  POST   /rate-limit/reset    - Reset rate limit\n")
	fmt.Printf("  GET    /api/data            - Sample protected endpoint\n")
	fmt.Printf("  POST   /api/data            - Sample protected endpoint\n")

	if err := router.Run(port); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

func handleGetData(c *gin.Context) {
	// Simulate slight processing delay
	time.Sleep(10 * time.Millisecond)

	c.JSON(http.StatusOK, gin.H{
		"data":      "Sample data from protected endpoint",
		"status":    "success",
		"timestamp": time.Now(),
	})
}

func handlePostData(c *gin.Context) {
	var payload map[string]interface{}
	if err := c.BindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"message": "Data created successfully",
		"data":    payload,
	})
}

func handleGetUser(c *gin.Context) {
	id := c.Param("id")
	c.JSON(http.StatusOK, gin.H{
		"id":         id,
		"name":       "User " + id,
		"created_at": time.Now(),
	})
}
