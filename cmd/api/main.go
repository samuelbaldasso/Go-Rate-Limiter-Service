package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"golang_rate_limiter/internal/limiter"
	"golang_rate_limiter/internal/middleware"
)

func main() {
	port := envOr("PORT", "8080")
	rate := envFloatOr("RATE_LIMIT_RPS", 5)
	burst := envFloatOr("RATE_LIMIT_BURST", 10)
	cleanupInterval := envDurationOr("RATE_LIMIT_CLEANUP_INTERVAL", time.Minute)
	visitorTTL := envDurationOr("RATE_LIMIT_VISITOR_TTL", 3*time.Minute)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	l := limiter.New(rate, burst)
	l.StartCleanup(ctx, cleanupInterval, visitorTTL)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	handler := middleware.RateLimit(l, mux)

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      handler,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("listening on :%s (rate=%.2f/s burst=%.2f)", port, rate, burst)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("graceful shutdown failed: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envFloatOr(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Fatalf("invalid %s: %v", key, err)
	}
	return f
}

func envDurationOr(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Fatalf("invalid %s: %v", key, err)
	}
	return d
}
