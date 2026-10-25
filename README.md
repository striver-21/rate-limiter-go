# rate-limiter-go

[![ci](https://github.com/striver-21/rate-limiter-go/actions/workflows/ci.yml/badge.svg)](https://github.com/striver-21/rate-limiter-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/striver-21/rate-limiter-go.svg)](https://pkg.go.dev/github.com/striver-21/rate-limiter-go)

A sliding-window-log rate limiter for Go services, with a Redis backend for
fleets that share one limit and an in-memory backend for single instances
and tests. Ships as `net/http` middleware with a Gin adapter, and sets the
IETF draft `RateLimit-*` headers.

**Live demo** (1 OCPU Oracle Cloud VM, Redis backend, 100 req/min per IP):

```bash
curl -i http://137.23.51.193/ratelimiter/api/data
```

## Why a sliding window log

| Algorithm | Burst at window edge | Memory per key | Exactness |
|---|---|---|---|
| Fixed window counter | up to 2× limit | O(1) | approximate |
| Sliding window counter | small | O(1) | approximate |
| **Sliding window log** (this) | none | O(limit) | exact |
| Token bucket / GCRA | configurable | O(1) | exact for its own model |

The log is exact and easy to reason about; its cost is one sorted-set entry
per request, which is fine for per-user/per-IP limits in the hundreds or
thousands per window. For limits in the millions per window, use a counter
or GCRA instead — that trade-off is the point of the interface.

## How the Redis backend works

One Lua script, one round trip, atomic per key:

```
ZREMRANGEBYSCORE key -inf (now - window)   -- drop expired entries
count = ZCARD key
if count < limit: ZADD key now <unique-member>; count++
PEXPIRE key window + 1s                    -- let idle keys disappear
reset = oldest score + window - now        -- when the next slot frees up
return {allowed, count, reset}
```

Scores are microseconds. The member is `timestamp-random64`, so two requests
in the same microsecond can't overwrite each other (the original version used
Lua's `math.random`, which is deterministically seeded on older Redis).

```
          ┌─────────────┐   Allow(ctx,key)   ┌──────────────────┐
request ─▶│ httpmw.Limit│───────────────────▶│ ratelimit.Limiter│
          │  (headers,  │◀───────────────────│  Redis | Memory  │
          │ 429/503)    │  Result{Allowed,   └──────────────────┘
          └─────────────┘   Remaining,Reset}
```

## Usage

```go
import (
    "github.com/redis/go-redis/v9"
    "github.com/striver-21/rate-limiter-go/httpmw"
    "github.com/striver-21/rate-limiter-go/ratelimit"
)

rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
limiter, err := ratelimit.NewRedis(rdb, ratelimit.Config{Limit: 100, Window: time.Minute})

// net/http
mux := http.NewServeMux()
mux.Handle("/api/", httpmw.Limit(limiter, httpmw.Options{
    Key:      httpmw.TrustedProxy("X-Forwarded-For", "10.0.0.0/8"), // or httpmw.ClientIP
    FailOpen: false, // 503 when Redis is unreachable; true lets traffic through
})(apiHandler))

// Gin
r.Use(ginmw.Limit(limiter, httpmw.Options{}))

// anywhere else
res, err := limiter.Allow(ctx, "apikey:"+key)
if !res.Allowed { /* res.RetryAfter */ }
```

Response headers on every limited route:

```
RateLimit-Limit: 100
RateLimit-Remaining: 97
RateLimit-Reset: 42          # seconds until the oldest request leaves the window
Retry-After: 42              # only on 429
```

### Key selection and proxies

`httpmw.ClientIP` uses the TCP peer address and ignores `X-Forwarded-For`.
Behind a load balancer use `httpmw.TrustedProxy(header, cidrs...)`, which
honours the header only when the direct peer is in a trusted CIDR — otherwise
clients could pick their own bucket.

### Failure mode

Redis down ⇒ `Allow` returns an error. The middleware then returns **503**
(default) or passes the request through when `FailOpen: true`. Choose
deliberately: fail-closed protects the upstream, fail-open protects
availability.

## Demo server

```bash
docker compose up --build          # Redis + server on :8080
# or, in-memory, no Redis:
go run ./cmd/server
```

| Env | Default | Meaning |
|---|---|---|
| `PORT` | `8080` | listen port |
| `REDIS_ADDR` | *(empty = in-memory)* | `host:port` of Redis |
| `RATE_LIMIT` | `100` | requests per window |
| `RATE_WINDOW` | `1m` | window (`1s`, `30s`, `1m`, …) |
| `FAIL_OPEN` | `false` | allow traffic when Redis errors |
| `TRUSTED_PROXIES` | *(none)* | CIDRs whose `X-Forwarded-For` is trusted |
| `ADMIN_TOKEN` | *(unset = disabled)* | enables `POST /admin/reset?key=…` with `Authorization: Bearer` |

Routes: `GET /health`, `GET /api/data`, `GET /api/users/:id` (limited),
`POST /admin/reset` (only with `ADMIN_TOKEN`). Structured JSON logs via
`log/slog`; graceful shutdown on SIGINT/SIGTERM.

## Tests and benchmarks

```bash
go test -race ./...
go test -bench . -benchmem ./ratelimit/
```

Tests cover: allow-then-deny at the limit, the window actually sliding (a
slot frees exactly `window` after its request, not at a bucket boundary),
key isolation, 20 goroutines × 25 calls never exceeding the limit, Redis
unavailable, key TTL, header values, trusted-proxy spoofing, fail-open vs
fail-closed. The Redis implementation is tested against
[miniredis](https://github.com/alicebob/miniredis) so CI needs no Redis.

Measured on this laptop (i5-8250U, Go 1.26, `go test -bench`):

| Benchmark | ns/op | allocs/op |
|---|---|---|
| `MemoryAllow` (single goroutine) | 90 | 0 |
| `MemoryAllowParallel` (8 goroutines, 64 keys) | 226 | 0 |

The in-process Redis benchmark against miniredis is **not** reported as a
throughput figure: miniredis interprets Lua in Go and is far slower than a
real Redis, so the number measures the test double, not the limiter.

### Load test against real Redis

Run on the live-demo VM itself (Oracle Cloud `VM.Standard.E2.1.Micro`:
**1 OCPU, 1 GB RAM**, Redis 7 and the server as containers on the same host,
`hey -z 30s -c 50`, one client key, limit 100/min):

```
Requests/sec:  1021
Latency:       p50 18 ms · p90 94 ms · p99 110 ms · max 282 ms
Responses:     [200] 100   [429] 30719
```

Two things worth reading off that: the limiter allowed **exactly 100**
requests in the window under 50 concurrent connections (the atomic Lua
script doing its job), and ~1k req/s is the ceiling of a shared 1-vCPU box
running the load generator, Redis and the server together — not of the
limiter. On real hardware the cost per request is one Redis round trip.

## Layout

```
ratelimit/   Limiter interface, Redis and in-memory implementations, tests, benchmarks
httpmw/      net/http middleware, RateLimit headers, key functions
ginmw/       Gin adapter
cmd/server/  demo API (the live demo above)
```

## Not implemented (yet)

- Sliding-window **counter** / GCRA backends for very high limits
- Per-route limits in the demo server (the package supports it; the demo uses one)
- Redis Cluster hash-tagging helpers (works with cluster clients as long as the key lands on one slot)

## License

MIT
