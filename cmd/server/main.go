// Command server is a small demo API protected by the rate limiter.
//
// Configuration (environment variables):
//
//	PORT            listen port                       (default 8080)
//	REDIS_ADDR      host:port; empty = in-memory      (default "")
//	RATE_LIMIT      requests per window               (default 100)
//	RATE_WINDOW     window duration, e.g. 1s, 1m      (default 1m)
//	FAIL_OPEN       allow traffic if Redis is down    (default false)
//	TRUSTED_PROXIES CIDRs whose X-Forwarded-For to trust, comma-separated
//	ADMIN_TOKEN     if set, enables POST /admin/reset with Bearer auth
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/striver-21/rate-limiter-go/ginmw"
	"github.com/striver-21/rate-limiter-go/httpmw"
	"github.com/striver-21/rate-limiter-go/ratelimit"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	limiter, rdb, err := buildLimiter(ctx, cfg)
	if err != nil {
		return err
	}
	if rdb != nil {
		defer rdb.Close()
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	if err := r.SetTrustedProxies(cfg.trustedProxies); err != nil {
		return fmt.Errorf("trusted proxies: %w", err)
	}

	keyFn := httpmw.ClientIP
	if len(cfg.trustedProxies) > 0 {
		keyFn = httpmw.TrustedProxy("X-Forwarded-For", cfg.trustedProxies...)
	}

	r.GET("/health", func(c *gin.Context) {
		if rdb != nil {
			if err := rdb.Ping(c.Request.Context()).Err(); err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unhealthy", "redis": err.Error()})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "healthy", "backend": cfg.backendName(), "limit": cfg.limit, "window": cfg.window.String()})
	})

	api := r.Group("/api", ginmw.Limit(limiter, httpmw.Options{Key: keyFn, FailOpen: cfg.failOpen, Logger: log}))
	api.GET("/data", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"data": "sample data from a rate-limited endpoint", "timestamp": time.Now().UTC()})
	})
	api.GET("/users/:id", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"id": c.Param("id"), "name": "User " + c.Param("id")})
	})

	if cfg.adminToken != "" {
		r.POST("/admin/reset", func(c *gin.Context) {
			if c.GetHeader("Authorization") != "Bearer "+cfg.adminToken {
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
			key := c.Query("key")
			if key == "" || rdb == nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "key required and Redis backend needed"})
				return
			}
			if err := rdb.Del(c.Request.Context(), "ratelimit:"+key).Err(); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.Status(http.StatusNoContent)
		})
	}

	srv := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr, "backend", cfg.backendName(), "limit", cfg.limit, "window", cfg.window.String(), "fail_open", cfg.failOpen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

type config struct {
	port           string
	redisAddr      string
	limit          int64
	window         time.Duration
	failOpen       bool
	trustedProxies []string
	adminToken     string
}

func (c config) backendName() string {
	if c.redisAddr == "" {
		return "memory"
	}
	return "redis"
}

func loadConfig() (config, error) {
	c := config{
		port:       envOr("PORT", "8080"),
		redisAddr:  os.Getenv("REDIS_ADDR"),
		adminToken: os.Getenv("ADMIN_TOKEN"),
	}
	var err error
	if c.limit, err = strconv.ParseInt(envOr("RATE_LIMIT", "100"), 10, 64); err != nil {
		return c, fmt.Errorf("RATE_LIMIT: %w", err)
	}
	if c.window, err = time.ParseDuration(envOr("RATE_WINDOW", "1m")); err != nil {
		return c, fmt.Errorf("RATE_WINDOW: %w", err)
	}
	if c.failOpen, err = strconv.ParseBool(envOr("FAIL_OPEN", "false")); err != nil {
		return c, fmt.Errorf("FAIL_OPEN: %w", err)
	}
	if v := os.Getenv("TRUSTED_PROXIES"); v != "" {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				c.trustedProxies = append(c.trustedProxies, p)
			}
		}
	}
	return c, nil
}

func buildLimiter(ctx context.Context, cfg config) (ratelimit.Limiter, *redis.Client, error) {
	rc := ratelimit.Config{Limit: cfg.limit, Window: cfg.window}
	if cfg.redisAddr == "" {
		l, err := ratelimit.NewMemory(rc)
		return l, nil, err
	}
	rdb := redis.NewClient(&redis.Options{Addr: cfg.redisAddr})
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		if !cfg.failOpen {
			return nil, nil, fmt.Errorf("redis %s: %w", cfg.redisAddr, err)
		}
		slog.Warn("redis unreachable at startup; continuing because FAIL_OPEN=true", "err", err)
	}
	l, err := ratelimit.NewRedis(rdb, rc)
	return l, rdb, err
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
