// Package ginmw adapts the net/http middleware in httpmw to Gin.
package ginmw

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/striver-21/rate-limiter-go/httpmw"
	"github.com/striver-21/rate-limiter-go/ratelimit"
)

// Limit returns a Gin middleware. Options are the same as httpmw.Options;
// if Key is nil the client IP is used (Gin's c.ClientIP honours trusted
// proxies configured on the engine, so prefer that over X-Forwarded-For).
func Limit(l ratelimit.Limiter, opts httpmw.Options) gin.HandlerFunc {
	if opts.Key == nil {
		opts.Key = httpmw.ClientIP
	}
	mw := httpmw.Limit(l, opts)
	return func(c *gin.Context) {
		allowed := false
		mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			allowed = true
			c.Request = r
		})).ServeHTTP(c.Writer, c.Request)
		if !allowed {
			c.Abort()
			return
		}
		c.Next()
	}
}

// KeyFromContext builds a KeyFunc that prefers a value set earlier in the
// Gin context (e.g. an authenticated user ID) and falls back to fallback.
// Because httpmw works on *http.Request, the value is carried via header
// shadowing: set it with c.Set(ctxKey, value) before this middleware runs
// and pass the same ctxKey here.
func KeyFromContext(c *gin.Context, ctxKey string, fallback httpmw.KeyFunc) string {
	if v := c.GetString(ctxKey); v != "" {
		return v
	}
	return fallback(c.Request)
}
