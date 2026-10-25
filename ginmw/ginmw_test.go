package ginmw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/striver-21/rate-limiter-go/httpmw"
	"github.com/striver-21/rate-limiter-go/ratelimit"
)

func TestGinMiddlewareLimitsAndAborts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	l, err := ratelimit.NewMemory(ratelimit.Config{Limit: 1, Window: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	r := gin.New()
	r.GET("/x", Limit(l, httpmw.Options{}), func(c *gin.Context) {
		calls++
		c.String(http.StatusOK, "ok")
	})

	do := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil)
		req.RemoteAddr = "192.0.2.1:1234"
		r.ServeHTTP(rec, req)
		return rec
	}

	if rec := do(); rec.Code != http.StatusOK {
		t.Fatalf("first request: status %d, want 200", rec.Code)
	}
	rec := do()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After on 429")
	}
	if calls != 1 {
		t.Errorf("handler ran %d times, want 1 (chain must be aborted on deny)", calls)
	}
}
