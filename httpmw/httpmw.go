// Package httpmw adapts a ratelimit.Limiter to net/http middleware and sets
// the IETF draft RateLimit response headers
// (https://datatracker.ietf.org/doc/draft-ietf-httpapi-ratelimit-headers/).
package httpmw

import (
	"context"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/striver-21/rate-limiter-go/ratelimit"
)

// KeyFunc extracts the rate-limit key from a request, e.g. client IP, API
// key or user ID. Returning "" skips limiting for that request.
type KeyFunc func(*http.Request) string

// Options configure the middleware.
type Options struct {
	// Key chooses what to limit on. Default: ClientIP.
	Key KeyFunc
	// FailOpen lets requests through when the limiter backend errors
	// (e.g. Redis is down). Default false: respond 503.
	FailOpen bool
	// OnDenied writes the 429 response. Default: JSON body with Retry-After.
	OnDenied http.HandlerFunc
	// Logger receives backend errors. Default slog.Default().
	Logger *slog.Logger
}

// ClientIP returns the remote address without port. It deliberately does
// NOT trust X-Forwarded-For; wrap it in TrustedProxy when behind one.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// TrustedProxy returns a KeyFunc that uses the given header (typically
// X-Forwarded-For or X-Real-IP) only when the direct peer is one of the
// trusted CIDRs; otherwise it falls back to the peer address. This stops
// clients from spoofing their own key.
func TrustedProxy(header string, cidrs ...string) KeyFunc {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			nets = append(nets, n)
		}
	}
	return func(r *http.Request) string {
		peer := net.ParseIP(ClientIP(r))
		trusted := false
		for _, n := range nets {
			if peer != nil && n.Contains(peer) {
				trusted = true
				break
			}
		}
		if !trusted {
			return ClientIP(r)
		}
		if v := r.Header.Get(header); v != "" {
			// X-Forwarded-For is "client, proxy1, proxy2": the first hop is the client.
			if i := indexByte(v, ','); i >= 0 {
				v = v[:i]
			}
			return trimSpace(v)
		}
		return ClientIP(r)
	}
}

// Limit wraps next with rate limiting.
func Limit(l ratelimit.Limiter, opts Options) func(http.Handler) http.Handler {
	if opts.Key == nil {
		opts.Key = ClientIP
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.OnDenied == nil {
		opts.OnDenied = defaultDenied
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := opts.Key(r)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			res, err := l.Allow(r.Context(), key)
			if err != nil {
				opts.Logger.ErrorContext(r.Context(), "rate limiter backend error", "err", err, "key", key)
				if opts.FailOpen {
					next.ServeHTTP(w, r)
					return
				}
				http.Error(w, `{"error":"rate limiter unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			SetHeaders(w.Header(), res)
			if !res.Allowed {
				opts.OnDenied(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// SetHeaders writes RateLimit-Limit, RateLimit-Remaining, RateLimit-Reset
// and, when denied, Retry-After.
func SetHeaders(h http.Header, res ratelimit.Result) {
	h.Set("RateLimit-Limit", strconv.FormatInt(res.Limit, 10))
	h.Set("RateLimit-Remaining", strconv.FormatInt(res.Remaining, 10))
	h.Set("RateLimit-Reset", strconv.Itoa(ceilSeconds(res.ResetAfter)))
	if !res.Allowed {
		h.Set("Retry-After", strconv.Itoa(ceilSeconds(res.RetryAfter)))
	}
}

func defaultDenied(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
}

func ceilSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(math.Ceil(d.Seconds()))
}

// tiny helpers so this package does not import strings for two calls.
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// Allow is a convenience for handlers that want the decision without the
// middleware (e.g. to limit an expensive sub-operation).
func Allow(ctx context.Context, l ratelimit.Limiter, key string) (ratelimit.Result, error) {
	return l.Allow(ctx, key)
}
