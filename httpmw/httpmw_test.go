package httpmw

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/striver-21/rate-limiter-go/ratelimit"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestLimitSetsHeadersAndDenies(t *testing.T) {
	l, err := ratelimit.NewMemory(ratelimit.Config{Limit: 2, Window: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	h := Limit(l, Options{})(okHandler())

	for i, wantStatus := range []int{200, 200, 429} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.5:4321"
		h.ServeHTTP(rec, req)
		if rec.Code != wantStatus {
			t.Fatalf("request %d: status %d, want %d", i+1, rec.Code, wantStatus)
		}
		if got := rec.Header().Get("RateLimit-Limit"); got != "2" {
			t.Errorf("RateLimit-Limit = %q, want 2", got)
		}
	}
	// last response was denied
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:4321"
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Retry-After") == "" {
		t.Error("denied response missing Retry-After")
	}
	if rec.Header().Get("RateLimit-Remaining") != "0" {
		t.Errorf("RateLimit-Remaining = %q, want 0", rec.Header().Get("RateLimit-Remaining"))
	}
}

func TestDifferentIPsDifferentBuckets(t *testing.T) {
	l, _ := ratelimit.NewMemory(ratelimit.Config{Limit: 1, Window: time.Minute})
	h := Limit(l, Options{})(okHandler())
	for _, addr := range []string{"10.0.0.1:1", "10.0.0.2:1"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = addr
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", addr, rec.Code)
		}
	}
}

func TestTrustedProxyOnlyHonoursHeaderFromTrustedPeer(t *testing.T) {
	key := TrustedProxy("X-Forwarded-For", "127.0.0.0/8")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:9000"
	req.Header.Set("X-Forwarded-For", "198.51.100.7, 10.1.1.1")
	if got := key(req); got != "198.51.100.7" {
		t.Errorf("trusted peer: key = %q, want 198.51.100.7", got)
	}

	req.RemoteAddr = "203.0.113.9:9000" // untrusted peer spoofing the header
	if got := key(req); got != "203.0.113.9" {
		t.Errorf("untrusted peer: key = %q, want peer address", got)
	}
}

type failingLimiter struct{}

func (failingLimiter) Allow(context.Context, string) (ratelimit.Result, error) {
	return ratelimit.Result{}, errors.New("backend down")
}

func TestBackendErrorFailClosedAndOpen(t *testing.T) {
	for _, tc := range []struct {
		failOpen bool
		want     int
	}{{false, http.StatusServiceUnavailable}, {true, http.StatusOK}} {
		h := Limit(failingLimiter{}, Options{FailOpen: tc.failOpen})(okHandler())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != tc.want {
			t.Errorf("FailOpen=%v: status %d, want %d", tc.failOpen, rec.Code, tc.want)
		}
	}
}

func TestEmptyKeySkipsLimiting(t *testing.T) {
	l, _ := ratelimit.NewMemory(ratelimit.Config{Limit: 1, Window: time.Minute})
	h := Limit(l, Options{Key: func(*http.Request) string { return "" }})(okHandler())
	for range 5 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200", rec.Code)
		}
	}
}
