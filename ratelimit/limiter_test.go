package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// fakeClock lets tests move time deterministically.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type impl struct {
	name string
	// build returns a limiter wired to clk and a cleanup func.
	build func(t *testing.T, cfg Config, clk *fakeClock) (Limiter, *miniredis.Miniredis)
}

func impls() []impl {
	return []impl{
		{
			name: "memory",
			build: func(t *testing.T, cfg Config, clk *fakeClock) (Limiter, *miniredis.Miniredis) {
				t.Helper()
				l, err := NewMemory(cfg)
				if err != nil {
					t.Fatal(err)
				}
				l.now = clk.now
				return l, nil
			},
		},
		{
			name: "redis",
			build: func(t *testing.T, cfg Config, clk *fakeClock) (Limiter, *miniredis.Miniredis) {
				t.Helper()
				mr := miniredis.RunT(t)
				rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
				t.Cleanup(func() { _ = rdb.Close() })
				l, err := NewRedis(rdb, cfg)
				if err != nil {
					t.Fatal(err)
				}
				l.now = clk.now
				return l, mr
			},
		},
	}
}

func TestConfigValidation(t *testing.T) {
	for _, cfg := range []Config{{}, {Limit: 1}, {Window: time.Second}, {Limit: -1, Window: time.Second}} {
		if _, err := NewMemory(cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("NewMemory(%+v) err = %v, want ErrInvalidConfig", cfg, err)
		}
		if _, err := NewRedis(nil, cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("NewRedis(%+v) err = %v, want ErrInvalidConfig", cfg, err)
		}
	}
}

func TestAllowUpToLimitThenDeny(t *testing.T) {
	for _, im := range impls() {
		t.Run(im.name, func(t *testing.T) {
			clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
			l, _ := im.build(t, Config{Limit: 3, Window: time.Second}, clk)
			ctx := context.Background()

			for i := range 3 {
				res, err := l.Allow(ctx, "k")
				if err != nil {
					t.Fatal(err)
				}
				if !res.Allowed {
					t.Fatalf("request %d: denied, want allowed", i+1)
				}
				if want := int64(2 - i); res.Remaining != want {
					t.Errorf("request %d: Remaining = %d, want %d", i+1, res.Remaining, want)
				}
			}

			res, err := l.Allow(ctx, "k")
			if err != nil {
				t.Fatal(err)
			}
			if res.Allowed {
				t.Fatal("4th request allowed, want denied")
			}
			if res.Remaining != 0 {
				t.Errorf("Remaining = %d, want 0", res.Remaining)
			}
			if res.RetryAfter <= 0 || res.RetryAfter > time.Second {
				t.Errorf("RetryAfter = %v, want in (0, 1s]", res.RetryAfter)
			}
		})
	}
}

// The window must slide: a request made at t=0 frees its slot at t=1s,
// not at the next fixed bucket boundary.
func TestWindowSlides(t *testing.T) {
	for _, im := range impls() {
		t.Run(im.name, func(t *testing.T) {
			clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
			l, _ := im.build(t, Config{Limit: 2, Window: time.Second}, clk)
			ctx := context.Background()

			mustAllow(t, l, ctx, "k") // t=0
			clk.advance(600 * time.Millisecond)
			mustAllow(t, l, ctx, "k") // t=0.6
			mustDeny(t, l, ctx, "k")  // 2 in window

			clk.advance(450 * time.Millisecond) // t=1.05: the t=0 entry has expired
			res := mustAllow(t, l, ctx, "k")
			if res.Remaining != 0 {
				t.Errorf("Remaining = %d, want 0 (entries at 0.6 and 1.05)", res.Remaining)
			}
			mustDeny(t, l, ctx, "k") // t=0.6 entry still inside the window

			clk.advance(600 * time.Millisecond) // t=1.65: only the 1.05 entry remains
			mustAllow(t, l, ctx, "k")
		})
	}
}

func TestKeysAreIndependent(t *testing.T) {
	for _, im := range impls() {
		t.Run(im.name, func(t *testing.T) {
			clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
			l, _ := im.build(t, Config{Limit: 1, Window: time.Second}, clk)
			ctx := context.Background()
			mustAllow(t, l, ctx, "a")
			mustDeny(t, l, ctx, "a")
			mustAllow(t, l, ctx, "b")
		})
	}
}

// Many goroutines hammering one key must never exceed the limit in total.
func TestConcurrentCallersNeverExceedLimit(t *testing.T) {
	for _, im := range impls() {
		t.Run(im.name, func(t *testing.T) {
			const limit, callers, perCaller = 50, 20, 25
			clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
			l, _ := im.build(t, Config{Limit: limit, Window: time.Minute}, clk)
			ctx := context.Background()

			var allowed atomic.Int64
			var wg sync.WaitGroup
			for range callers {
				wg.Go(func() {
					for range perCaller {
						res, err := l.Allow(ctx, "shared")
						if err != nil {
							t.Error(err)
							return
						}
						if res.Allowed {
							allowed.Add(1)
						}
					}
				})
			}
			wg.Wait()
			if got := allowed.Load(); got != limit {
				t.Fatalf("allowed %d requests, want exactly %d", got, limit)
			}
		})
	}
}

func TestRedisUnavailableReturnsError(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	im := impls()[1]
	l, mr := im.build(t, Config{Limit: 1, Window: time.Second}, clk)
	mr.Close()
	if _, err := l.Allow(context.Background(), "k"); err == nil {
		t.Fatal("Allow with Redis down: err = nil, want error")
	}
}

func TestRedisKeyExpires(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	im := impls()[1]
	l, mr := im.build(t, Config{Limit: 1, Window: time.Second, KeyPrefix: "rl:"}, clk)
	mustAllow(t, l, context.Background(), "k")
	ttl := mr.TTL("rl:k")
	if ttl <= time.Second || ttl > 3*time.Second {
		t.Fatalf("TTL = %v, want window + ~1s", ttl)
	}
}

func mustAllow(t *testing.T, l Limiter, ctx context.Context, key string) Result {
	t.Helper()
	res, err := l.Allow(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatalf("Allow(%q) denied, want allowed (remaining=%d retryAfter=%v)", key, res.Remaining, res.RetryAfter)
	}
	return res
}

func mustDeny(t *testing.T, l Limiter, ctx context.Context, key string) Result {
	t.Helper()
	res, err := l.Allow(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed {
		t.Fatalf("Allow(%q) allowed, want denied", key)
	}
	return res
}

// Benchmarks. The Redis numbers below run against miniredis in-process and
// therefore measure the limiter's own overhead plus a loopback TCP round
// trip, not a production Redis. See README for a real load-test setup.

func BenchmarkMemoryAllow(b *testing.B) {
	l, err := NewMemory(Config{Limit: 1_000_000, Window: time.Second})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := l.Allow(ctx, "bench"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMemoryAllowParallel(b *testing.B) {
	l, err := NewMemory(Config{Limit: 1_000_000, Window: time.Second})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	var n atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		key := fmt.Sprintf("k%d", n.Add(1)%64)
		for pb.Next() {
			if _, err := l.Allow(ctx, key); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkRedisAllow(b *testing.B) {
	mr := miniredis.RunT(b)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	l, err := NewRedis(rdb, Config{Limit: 1_000_000, Window: time.Second})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := l.Allow(ctx, "bench"); err != nil {
			b.Fatal(err)
		}
	}
}
