package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"golang_rate_limiter/internal/limiter"
)

func TestRateLimit_AllowsWithinBurstThenBlocks(t *testing.T) {
	l := limiter.New(1, 2)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := RateLimit(l, next)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.1:12345"

	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, rr.Code)
		}
	}

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after burst exhausted, got %d", rr.Code)
	}
}

func TestRateLimit_XForwardedForOverridesRemoteAddr(t *testing.T) {
	l := limiter.New(1, 1)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := RateLimit(l, next)

	reqA := httptest.NewRequest(http.MethodGet, "/", nil)
	reqA.RemoteAddr = "203.0.113.1:1"
	reqA.Header.Set("X-Forwarded-For", "198.51.100.9, 10.0.0.1")

	reqB := httptest.NewRequest(http.MethodGet, "/", nil)
	reqB.RemoteAddr = "203.0.113.1:2" // same RemoteAddr, no XFF -> different key
	reqB.Header.Set("X-Forwarded-For", "198.51.100.10")

	rrA := httptest.NewRecorder()
	handler.ServeHTTP(rrA, reqA)
	if rrA.Code != http.StatusOK {
		t.Fatalf("expected 200 for client A, got %d", rrA.Code)
	}

	rrB := httptest.NewRecorder()
	handler.ServeHTTP(rrB, reqB)
	if rrB.Code != http.StatusOK {
		t.Fatalf("expected 200 for distinct XFF client B, got %d", rrB.Code)
	}
}

func TestClientIP_FallsBackToRemoteAddr(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:5555"

	if got := clientIP(req); got != "203.0.113.5" {
		t.Fatalf("expected 203.0.113.5, got %s", got)
	}
}
