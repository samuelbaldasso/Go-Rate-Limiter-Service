// Package middleware provides HTTP middleware, including rate limiting.
package middleware

import (
	"net"
	"net/http"
	"strings"

	"golang_rate_limiter/internal/limiter"
)

// RateLimit wraps next with rate limiting keyed by client IP.
func RateLimit(l *limiter.Limiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)

		if !l.Allow(ip) {
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// clientIP extracts the client IP from the request. It trusts the first
// address in X-Forwarded-For when present, falling back to RemoteAddr.
// NOTE: trusting X-Forwarded-For without a validated trusted-proxy chain in
// front of this service allows a client to spoof its rate-limit key; this is
// acceptable for this exercise but should be revisited before use behind an
// untrusted network path.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		parts := strings.Split(fwd, ",")
		if ip := strings.TrimSpace(parts[0]); ip != "" {
			return ip
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
