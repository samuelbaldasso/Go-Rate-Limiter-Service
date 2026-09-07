# Golang Rate Limiter

HTTP microservice in Go implementing per-client rate limiting using the
**Token Bucket** algorithm, built entirely with the standard library
(`net/http`, `sync`, `time`, `context`) — no external dependencies.

## Table of Contents

- [Overview](#overview)
- [Architecture](#architecture)
- [Directory structure](#directory-structure)
- [Configuration (environment variables)](#configuration-environment-variables)
- [Running it](#running-it)
- [Tests](#tests)
- [Observed behavior](#observed-behavior)
- [Known limitations](#known-limitations)
- [ADRs](#adrs)

## Overview

The service exposes an HTTP handler protected by a rate-limiting middleware.
Each client is identified by IP (with `X-Forwarded-For` support) and gets a
"bucket" of tokens that drains with each request and refills at a
configurable rate over time. When a client's bucket is empty, the service
responds with `429 Too Many Requests`; otherwise the request is passed
through to the final handler normally.

Each client's state (remaining tokens, timestamp of the last request) is
kept entirely in memory, in a `map[string]*visitor` protected by locks —
there's no dependency on a database or external cache.

## Architecture

```
                 ┌─────────────────────────┐
  Request -->    │  RateLimit middleware    │
                 │  (extracts client IP)    │
                 └─────────────┬────────────┘
                               │
                               v
                 ┌─────────────────────────┐
                 │  Limiter.Allow(ip)       │
                 │  - Global RWMutex        │
                 │    (lookup/create        │
                 │     visitor)             │
                 │  - Per-visitor Mutex     │
                 │    (token math)          │
                 └─────────────┬────────────┘
                               │
                 200 (allow)   │   429 (deny)
                               v
                 ┌─────────────────────────┐
                 │  Final handler (mux)     │
                 └─────────────────────────┘

  A cleanup goroutine (time.Ticker) runs in parallel,
  removing visitors inactive for longer than the configured TTL.
```

**Request flow:**

1. The `RateLimit` middleware extracts the client's IP (`internal/middleware/rate_limit.go`).
2. It calls `Limiter.Allow(ip)` (`internal/limiter/bucket.go`), which:
   - Looks up the `visitor` for that IP (read lock on the global map; write lock only if the visitor doesn't exist yet).
   - Locks the visitor's individual `Mutex`, calculates how many tokens have accumulated since the last request (`elapsed * rate`, capped at `burst`), and decrements a token if there's balance available.
3. If `Allow` returns `false`, the service responds `429`. Otherwise, the request is passed to the next handler.
4. In parallel, a cleanup goroutine (`StartCleanup`) periodically sweeps the map and removes visitors inactive for longer than the configured TTL, preventing memory leaks.

## Directory structure

```
cmd/api/main.go                    Entry point: starts the HTTP server,
                                    reads configuration from env vars,
                                    registers routes and the middleware,
                                    handles graceful shutdown.

internal/limiter/bucket.go         Token Bucket business logic:
                                    Visitor struct, Limiter struct, Allow(),
                                    StartCleanup().

internal/limiter/bucket_test.go    Unit tests for the limiter (burst,
                                    refill, concurrency, cleanup).

internal/middleware/rate_limit.go  HTTP middleware that wraps an
                                    http.Handler and consults the limiter
                                    before passing the request through.

internal/middleware/rate_limit_test.go
                                    Middleware tests via httptest.

Dockerfile                         Multi-stage build (golang:alpine
                                    builder -> scratch final image).

docker-compose.yml                 Runs the service locally with defaults.
```

## Configuration (environment variables)

| Variable                       | Default | Description                                                  |
|----------------------------------|---------|-----------------------------------------------------------------|
| `PORT`                          | `8080`  | Port the HTTP server listens on.                              |
| `RATE_LIMIT_RPS`                 | `5`     | Tokens (requests) refilled per second, per client.            |
| `RATE_LIMIT_BURST`               | `10`    | Maximum bucket capacity (number of burst requests allowed).   |
| `RATE_LIMIT_CLEANUP_INTERVAL`    | `1m`    | Interval between cleanup goroutine sweeps.                    |
| `RATE_LIMIT_VISITOR_TTL`         | `3m`    | Idle time after which a visitor is removed from the map.      |

## Running it

Via Docker Compose (recommended, no local Go install required):

```bash
docker compose up --build
```

Locally, with Go installed:

```bash
go run ./cmd/api
```

The service will be available at `http://localhost:8080`.

## Tests

```bash
go vet ./...
go test -race ./...
```

Or via Docker, without installing Go:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.22-alpine \
  sh -c "go vet ./... && go test -race ./..."
```

Current coverage:
- `internal/limiter`: burst and denial after exhaustion, refill over time, independence between distinct keys, concurrency on the same key (`-race`), removal of inactive visitors.
- `internal/middleware`: `429` response after exhausting burst, client isolation via `X-Forwarded-For`, IP-extraction fallback to `RemoteAddr`.

Not covered (out of scope for this delivery): load/benchmark testing and integration tests against a real container.

## Observed behavior

With the defaults (`RATE_LIMIT_RPS=5`, `RATE_LIMIT_BURST=10`), 12 requests fired back-to-back result in:

```
request 1..10: 200
request 11:    429
request 12:    429
```

After waiting ~1s, a new request is accepted again (`200`), since the bucket refills at 5 tokens/second.

## Known limitations

- **State not shared across replicas**: the visitor map lives in the process's memory. Running multiple replicas of the service behind a load balancer results in effective limits multiplied by the number of replicas (each keeps its own bucket per client). Correct distributed rate limiting would require a shared store (e.g., Redis) — out of scope for this exercise, which explicitly calls for in-memory state.
- **`X-Forwarded-For` trusted without proxy validation**: the service uses the first IP in the `X-Forwarded-For` header when present, without validating that the request actually came through a trusted proxy. A malicious client with direct access to the service can forge this header and bypass its per-IP limit. Acceptable for this exercise; a trusted reverse proxy in front (that overwrites/validates the header) would be required before use in production exposed to the internet.
- **Final image on `scratch`**: contains no `ca-certificates` and no shell. Sufficient for this service (it makes no outbound HTTPS calls), but would need adjustment (`ca-certificates`, or an `alpine` base) if the service starts making calls to external APIs over TLS.

## ADRs

### ADR-001: Token Bucket as the rate-limiting algorithm

**Status:** Accepted

**Context:** A per-client rate-limiting algorithm needs to be chosen. The options considered were Token Bucket, Fixed Window Counter, and Sliding Window Log/Counter.

**Decision:** Use Token Bucket, with a manual implementation (not `golang.org/x/time/rate`), computing accumulated tokens on demand from the `time.Duration` elapsed since the last request — no need for a dedicated per-client goroutine to "fill" the bucket.

**Alternatives discarded:**
- *Fixed Window Counter*: simpler, but allows bursts of up to 2x the limit at the boundary between windows (e.g., a burst at the end of one window plus a burst at the start of the next).
- *Sliding Window Log*: more precise, but would require storing per-client request timestamps, increasing memory usage and complexity with no real need for this use case.
- *`golang.org/x/time/rate`*: implements Token Bucket robustly and is well-tested, but was ruled out because the explicit requirement was to use only Go's standard library (`sync`, `time`) — the goal is also didactic, to exercise the concurrency logic by hand.

**Consequences:** Refill logic is *lazy* (computed at request time, not via a continuous per-client timer), which is more memory- and CPU-efficient for a large number of sparse clients, but requires care to avoid the bucket overflowing above the configured `burst` (mitigated with `if v.tokens > l.burst { v.tokens = l.burst }`).

---

### ADR-002: Lock granularity — global RWMutex + per-visitor Mutex

**Status:** Accepted

**Context:** Multiple goroutines can access and modify rate-limiting state concurrently — both for different clients and, occasionally, for the same client (simultaneous requests from the same IP).

**Decision:** Use two lock levels:
1. A `sync.RWMutex` on the `Limiter`, used with `RLock` for reads (the common path: visitor already exists) and `Lock` only when creating a new visitor (with double-checked locking to avoid a race between the `RUnlock` and the write `Lock`).
2. An individual `sync.Mutex` inside each `visitor`, protecting only that specific client's token calculation.

**Alternatives discarded:**
- *A single global Mutex* protecting reads, creation, and token math: simpler, but would serialize **all** requests to the service behind one lock, even between completely independent clients — a severe bottleneck under load.
- *`sync.Map`*: removes the need for the global `RWMutex` for concurrent map reads/writes, but was discarded because `sync.Map` is optimized for relatively stable keys with few writes and predominantly reads of distinct keys — our access pattern (frequent reads of the same key, made more explicit with `RWMutex`+plain map) is more predictable and easier to reason about correctness for in this didactic context.

**Consequences:** Requests for different clients don't compete for locks beyond the very brief `RLock` used to look them up in the map. Simultaneous requests for the **same** client are serialized only against each other (via that visitor's `Mutex`), which is the correct and expected behavior (token math can't run in parallel against the same state).

---

### ADR-003: Cleanup of inactive visitors via a `time.Ticker` goroutine

**Status:** Accepted

**Context:** The visitor map grows with every new IP seen and never shrinks on its own — in Go, maps don't expire keys automatically. Without cleanup, the service would leak memory indefinitely in production.

**Decision:** Run a background goroutine (`Limiter.StartCleanup`), started from `main.go`, that uses a `time.Ticker` to periodically sweep the map (`RATE_LIMIT_CLEANUP_INTERVAL`) and remove visitors whose `lastSeen` is beyond the configured TTL (`RATE_LIMIT_VISITOR_TTL`). The goroutine receives a `context.Context` and shuts down cleanly when the context is canceled (service shutdown).

**Alternatives discarded:**
- *Per-entry TTL with an individual timer (`time.AfterFunc` per visitor)*: more precise expiration timing, but creates the overhead of one timer per active client — unnecessary for this use case, and more complex to cancel correctly when a visitor becomes active again before expiring.
- *No cleanup (accept the leak)*: discarded as a known, avoidable design flaw, explicitly called out as a requirement in `task.md`.

**Consequences:** There's a window between a client's actual inactivity and its effective removal from the map (at most `TTL + CLEANUP_INTERVAL`), which is acceptable — the goal is to bound the map's growth, not to expire entries instantly.

---

### ADR-004: IP extraction with `X-Forwarded-For` support, without trusted-proxy validation

**Status:** Accepted, with a documented caveat

**Context:** In production, the service typically runs behind a reverse proxy or load balancer, which rewrites `RemoteAddr` to the proxy's IP, not the real client's. Without `X-Forwarded-For` support, rate limiting would end up grouping all clients under the proxy's IP.

**Decision:** If the `X-Forwarded-For` header is present, use the first IP in the list as the key; otherwise, fall back to `RemoteAddr`.

**Alternatives discarded:**
- *Ignore `X-Forwarded-For` and always use `RemoteAddr`*: safer against spoofing, but makes per-real-client rate limiting useless in any topology with a proxy/load balancer in front — a common production scenario.
- *Implement a trusted-proxy list and validate the `X-Forwarded-For` chain*: more correct and secure, but adds configuration complexity (trusted CIDR list) out of scope for this exercise.

**Consequences:** A client with direct access to the service (not going through a trusted proxy in front) can forge the `X-Forwarded-For` header and bypass the rate limit tied to their real IP. This is a conscious trade-off, documented in the [Known limitations](#known-limitations) section; this logic should be revisited before exposing the service directly to the internet without a trusted proxy in front.

---

### ADR-005: Graceful shutdown with `signal.NotifyContext`

**Status:** Accepted

**Context:** Without signal handling, a `SIGTERM` (for example, when stopping a container) would kill the process abruptly, cutting off in-flight connections and leaving the cleanup goroutine orphaned until the process dies.

**Decision:** Use `signal.NotifyContext` to capture `os.Interrupt` and `syscall.SIGTERM`, propagate that `context.Context` to the cleanup goroutine (which shuts down via `ctx.Done()`), and call `srv.Shutdown(shutdownCtx)` with a 5-second timeout upon receiving the signal.

**Alternatives discarded:**
- *No shutdown handling*: simpler, but not acceptable for a service meant to run in containers/orchestrators, where `SIGTERM` is the standard stop mechanism.

**Consequences:** In-flight requests at the moment of `SIGTERM` have up to 5 seconds to complete before the process exits; the cleanup goroutine stops deterministically alongside the server shutdown, with no goroutine leaks.

---

### ADR-006: Final Docker image on `scratch`

**Status:** Accepted, with a documented caveat

**Context:** The service needs to be packaged into a Docker image for deployment. The Go binary is statically compiled (`CGO_ENABLED=0`), which allows using a minimal final image.

**Decision:** Multi-stage build: `golang:1.22-alpine` to compile, `scratch` as the final image, copying only the binary.

**Alternatives discarded:**
- *`alpine` final image*: includes a shell and package manager, useful for debugging (interactive `docker exec`) and already ships `ca-certificates`, but increases the attack surface and image size unnecessarily, since the service makes no outbound HTTPS calls and needs no shell at runtime.

**Consequences:** Minimal final image (a few MB) with a reduced attack surface (no shell, no package manager, no dynamic libc). If the service ever needs to make HTTPS calls to external services, `ca-certificates` will need to be added (copied from `/etc/ssl/certs/ca-certificates.crt` in the build stage, or by switching the final image to `alpine`). Debugging in production becomes harder (no shell to `exec` into the container) — mitigated via structured logs on `stdout`.
