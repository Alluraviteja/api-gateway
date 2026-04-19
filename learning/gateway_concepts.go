// Package learning is a self-contained walkthrough of every concept used in this API gateway.
//
// Read this file top-to-bottom. Each section is a standalone, runnable idea.
// The concepts covered (in order):
//
//  1. http.Handler and http.HandlerFunc  — the foundation of everything
//  2. Middleware pattern                 — wrapping handlers to add behaviour
//  3. ResponseWriter wrapper             — capturing status code & size
//  4. Recovery middleware                — catching panics
//  5. Request ID middleware              — trace IDs via context
//  6. Security headers middleware        — hardening responses
//  7. Timeout middleware                 — context cancellation
//  8. Body size limiting                 — preventing memory exhaustion
//  9. Structured logging (slog)          — machine-readable access logs
// 10. Prometheus metrics                 — counters and histograms
// 11. Rate limiting (external service)   — fail-open HTTP client
// 12. Host-based routing                 — matching the Host header
// 13. Reverse proxy                      — forwarding requests to a backend
// 14. Circuit breaker                    — stopping cascading failures
// 15. Middleware chain composition       — wiring it all together
// 16. Health checks (liveness/readiness) — Kubernetes probes
// 17. Graceful shutdown                  — draining in-flight requests
// 18. TLS and HTTP→HTTPS redirect        — secure transport
// 19. Prometheus metrics endpoint        — exposing /metrics with auth
// 20. Configuration via environment vars — 12-factor app style
//
// NOTE: This file does NOT compile as-is because it imports packages from this
// module. Its purpose is to be READ as annotated reference code.
package learning

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sony/gobreaker"
)

// =============================================================================
// CONCEPT 1: http.Handler — the foundation
// Source: Go standard library (net/http)
//         Used throughout — e.g. cmd/main.go:58-63
// =============================================================================
//
// Every HTTP handler in Go implements this single interface:
//
//   type Handler interface {
//       ServeHTTP(ResponseWriter, *Request)
//   }
//
// http.HandlerFunc is a function type that satisfies http.Handler.
// This lets you write a plain function and use it as a handler — no struct needed.

var helloHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	// w — write headers and body back to the client
	// r — the incoming request (method, URL, headers, body, context)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("hello"))
})

// =============================================================================
// CONCEPT 2: Middleware pattern
// Source: middleware/recovery.go:9  (simplest example — one function, one job)
// =============================================================================
//
// A middleware is a function that:
//   - accepts an http.Handler (the "next" step)
//   - returns a new http.Handler that does something BEFORE/AFTER calling next
//
// This is the "decorator" or "chain of responsibility" pattern.
// It keeps each concern (logging, auth, tracing) in its own function.
//
//   incoming request
//        │
//   [middleware A]  ← runs first, calls next
//        │
//   [middleware B]  ← runs second, calls next
//        │
//   [actual handler]
//        │
//   [middleware B]  ← resumes after next returns (e.g. logs status)
//        │
//   [middleware A]  ← resumes last (e.g. catches panics)

func exampleMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Code here runs BEFORE the next handler
		fmt.Println("before")

		next.ServeHTTP(w, r) // hand off to the next middleware or final handler

		// Code here runs AFTER the next handler returns
		fmt.Println("after")
	})
}

// =============================================================================
// CONCEPT 3: ResponseWriter wrapper — capturing status code & response size
// Source: middleware/logger.go:10-30
// =============================================================================
//
// http.ResponseWriter does not expose the status code after it is written.
// To log or measure it, we wrap the writer and intercept WriteHeader and Write.
//
// This pattern appears in both the Logger and Metrics middleware.

type responseWriter struct {
	http.ResponseWriter        // embed — passes all other calls through unchanged
	status              int    // captured status code
	size                int    // total bytes written in the body
	written             bool   // guards against double WriteHeader calls
}

// WriteHeader intercepts the status code before delegating to the real writer.
func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.written = true
	rw.ResponseWriter.WriteHeader(status)
}

// Write accumulates the body size and defaults status to 200 if WriteHeader was never called.
func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.written {
		// http.ResponseWriter implicitly sends 200 on first Write if no WriteHeader call.
		rw.status = http.StatusOK
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.size += n
	return n, err
}

// =============================================================================
// CONCEPT 4: Recovery middleware — catching panics
// Source: middleware/recovery.go:9-24
// =============================================================================
//
// A panic anywhere in a handler goroutine will crash the entire server unless
// recovered. The Recovery middleware uses defer+recover to intercept any panic,
// log it, and return a 500 to the client instead of crashing.
//
// This must be the OUTERMOST middleware so it wraps everything else.

func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				// Log the panic with context so it is easy to find in logs.
				slog.Error("panic recovered",
					"error", err,
					"method", r.Method,
					"path", r.URL.Path,
					"host", r.Host,
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// =============================================================================
// CONCEPT 5: Request ID middleware — distributed tracing via context
// Source: middleware/request_id.go:17-28  (RequestID func)
//         middleware/request_id.go:10-13  (contextKey type + constant)
// =============================================================================
//
// A request ID is a unique token attached to every request. It ties together all
// log lines for a single request, even across multiple services.
//
// Two key Go patterns demonstrated here:
//   a) context.WithValue — attaching values to the request context
//   b) typed context key  — using a private type to avoid key collisions

// contextKey is an unexported type. Using a custom type (not a raw string) means
// no other package can accidentally overwrite our context value.
type contextKey string

const RequestIDKey contextKey = "request_id"

func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Honour an existing ID from the caller (useful for tracing across services).
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = generateID() // generate a cryptographically random 8-byte hex string
		}

		// Attach ID to context so any downstream handler can read it.
		ctx := context.WithValue(r.Context(), RequestIDKey, id)

		// Echo it back in the response so clients can correlate their own logs.
		w.Header().Set("X-Request-ID", id)

		// r.WithContext returns a shallow copy of r with the new context.
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b) // crypto/rand — not math/rand — for unpredictability
	return fmt.Sprintf("%x", b)
}

// =============================================================================
// CONCEPT 6: Security headers middleware
// Source: middleware/security_headers.go:6-15
// =============================================================================
//
// These HTTP response headers instruct the browser to apply defence-in-depth
// protections. They cost nothing and should be on every response.
//
//   X-Content-Type-Options: nosniff
//     Stops browsers from "sniffing" a different content type than declared.
//
//   X-Frame-Options: DENY
//     Prevents the page being embedded in an iframe (clickjacking defence).
//
//   X-XSS-Protection: 1; mode=block
//     Activates the browser's built-in XSS filter (legacy browsers).
//
//   Referrer-Policy: strict-origin-when-cross-origin
//     Limits how much of the URL is sent in the Referer header.
//
//   Content-Security-Policy: default-src 'self'
//     Restricts which resources the browser may load.

func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'")
		next.ServeHTTP(w, r)
	})
}

// =============================================================================
// CONCEPT 7: Timeout middleware — context cancellation
// Source: middleware/timeout.go:11-17
// =============================================================================
//
// A slow or hung backend can hold a connection open indefinitely.
// context.WithTimeout adds a deadline to the request context.
//
// When the deadline passes, ctx.Done() is closed and ctx.Err() returns
// context.DeadlineExceeded. Well-behaved code (net/http, database/sql, etc.)
// checks the context and aborts early when it is cancelled.
//
// defer cancel() is critical: it releases resources even if the request finishes
// before the timeout fires.

func Timeout(duration time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), duration)
		defer cancel() // always release the timer, even on early return

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// =============================================================================
// CONCEPT 8: Body size limiting — preventing memory exhaustion
// Source: middleware/body_limit.go:9-14
// =============================================================================
//
// Without a body size limit, a client can POST an arbitrarily large body and
// exhaust server memory. http.MaxBytesReader wraps r.Body and returns an error
// once `limit` bytes have been read. net/http then returns 413 automatically.

func MaxBodySize(limit int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// =============================================================================
// CONCEPT 9: Structured logging with slog
// Source: middleware/logger.go:33-52  (Logger middleware)
//         cmd/main.go:22-34          (logger initialisation with level)
// =============================================================================
//
// Structured logs emit key-value pairs rather than free-form text.
// This makes them machine-parseable by log aggregators (Datadog, Loki, etc.).
//
// slog.Info("request", "key", value, "key2", value2)
// → {"level":"INFO","msg":"request","key":value,"key2":value2}
//
// The responseWriter wrapper (Concept 3) lets us log the final status code
// AFTER the handler chain has run, even though we started timing BEFORE it ran.

func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rw, r) // run the rest of the chain

		// Everything below executes AFTER the response is written.
		requestID, _ := r.Context().Value(RequestIDKey).(string)
		slog.Info("request",
			"request_id", requestID,
			"method", r.Method,
			"host", r.Host,
			"path", r.URL.Path,
			"status", rw.status,
			"bytes", rw.size,
			"duration_ms", time.Since(start).Milliseconds(),
			"client_ip", remoteIP(r),
		)
	})
}

// remoteIP extracts the real client IP, respecting the X-Forwarded-For header
// set by load balancers and proxies in front of us.
func remoteIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		// X-Forwarded-For can be a comma-separated list; the first IP is the client.
		return strings.SplitN(ip, ",", 2)[0]
	}
	// r.RemoteAddr is "IP:port" — strip the port.
	if idx := strings.LastIndex(r.RemoteAddr, ":"); idx != -1 {
		return r.RemoteAddr[:idx]
	}
	return r.RemoteAddr
}

// =============================================================================
// CONCEPT 10: Prometheus metrics
// Source: middleware/metrics.go:12-28  (metric declarations)
//         middleware/metrics.go:31-48  (Metrics middleware)
// =============================================================================
//
// Prometheus is a pull-based metrics system. Your server exposes a /metrics
// endpoint; Prometheus scrapes it on a schedule.
//
// Three metric types used here:
//
//   Counter    — monotonically increasing number (total requests, total errors)
//   Histogram  — samples observations into buckets (request latency distribution)
//   Labels     — dimensions on a metric (e.g. per-host, per-method, per-status)
//
// promauto.New* registers the metric automatically at init time.
// WithLabelValues selects the right time series for a given label combination.

var (
	// gateway_requests_total{host="...", method="GET", status="200"} counter
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_requests_total",
		Help: "Total number of requests processed by the gateway.",
	}, []string{"host", "method", "status"})

	// gateway_request_duration_seconds{host="...", method="GET"} histogram
	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_request_duration_seconds",
		Help:    "Request duration in seconds.",
		Buckets: prometheus.DefBuckets, // .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10
	}, []string{"host", "method"})

	// gateway_rate_limited_total{host="..."} counter
	rateLimitedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_rate_limited_total",
		Help: "Total number of requests rejected by rate limiting.",
	}, []string{"host"})
)

func Metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rw, r)

		duration := time.Since(start).Seconds()
		status := strconv.Itoa(rw.status)

		requestsTotal.WithLabelValues(r.Host, r.Method, status).Inc()
		requestDuration.WithLabelValues(r.Host, r.Method).Observe(duration)

		if rw.status == http.StatusTooManyRequests {
			rateLimitedTotal.WithLabelValues(r.Host).Inc()
		}
	})
}

// =============================================================================
// CONCEPT 11: Rate limiting via an external service — fail-open policy
// Source: client/rate_limiter_client.go:30-73  (client + IsAllowed)
//         middleware/rate_limit.go:13-42        (RateLimit middleware)
// =============================================================================
//
// This gateway delegates rate limit decisions to a separate service.
// The client sends a POST with {clientId, appId, endpoint} and the service
// responds with {allowed: true/false}.
//
// FAIL-OPEN: if the rate limiter is unreachable, IsAllowed returns true.
// Rationale: availability is more important than strict enforcement.
// A brief outage of the rate limiter should not take down all traffic.
//
// Connection pooling: the http.Client reuses TCP connections across calls.
// Without this, every rate limit check opens a new TCP connection (expensive).

type RateLimiterClient struct {
	baseURL    string
	httpClient *http.Client
}

type checkRequest struct {
	ClientID string `json:"clientId"`
	AppID    string `json:"appId"`
	Endpoint string `json:"endpoint"`
}

type checkResponse struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

func NewRateLimiterClient(baseURL string) *RateLimiterClient {
	return &RateLimiterClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 2 * time.Second, // tight timeout — don't let rate limiter slow us down
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (c *RateLimiterClient) IsAllowed(clientID, appID, endpoint string) (bool, error) {
	body, _ := json.Marshal(checkRequest{ClientID: clientID, AppID: appID, Endpoint: endpoint})

	resp, err := c.httpClient.Post(c.baseURL+"/rate-limit/check", "application/json", bytes.NewReader(body))
	if err != nil {
		// FAIL-OPEN: treat the request as allowed if we cannot reach the service.
		return true, fmt.Errorf("rate limiter unreachable: %w", err)
	}
	defer resp.Body.Close()

	var result checkResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return true, fmt.Errorf("decode response: %w", err) // fail-open on decode errors too
	}
	return result.Allowed, nil
}

// Ping is used by the readiness probe to check if the rate limiter is reachable.
// Any HTTP response means the server is up — we don't care about the status code.
func (c *RateLimiterClient) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, c.baseURL, nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("rate limiter unreachable: %w", err)
	}
	resp.Body.Close()
	return nil
}

// RateLimit middleware — sits in the chain and calls IsAllowed for known hosts.
// Unknown hosts (not in the routes map) skip rate limiting entirely.
func RateLimit(rl *RateLimiterClient, routes map[string]string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If this host is not in our routes, skip rate limiting.
		if _, ok := routes[r.Host]; !ok {
			next.ServeHTTP(w, r)
			return
		}

		// Prefer the X-Client-ID header; fall back to the client IP.
		clientID := r.Header.Get("X-Client-ID")
		if clientID == "" {
			clientID = remoteIP(r)
		}

		// Derive appId from the subdomain: "app1.test.com" → "app1"
		appID := strings.SplitN(r.Host, ".", 2)[0]

		allowed, err := rl.IsAllowed(clientID, appID, r.URL.Path)
		if err != nil {
			slog.Warn("rate limiter error — failing open", "error", err)
			// Fail-open: allow the request through even on error.
		}

		if !allowed {
			http.Error(w, "429 Too Many Requests", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// =============================================================================
// CONCEPT 12: Host-based routing
// Source: router/router.go:13-47
// =============================================================================
//
// The gateway decides which backend to send a request to based on the Host
// header. This is the same mechanism virtual hosts use in nginx/Apache.
//
//   Host: app1.test.com  →  http://app1-service
//   Host: app2.test.com  →  http://app2-service
//   Host: unknown.com    →  502 (or passthrough if ALLOW_PASSTHROUGH=true)
//
// handlers is built once at startup; each lookup is O(1) map access.

func NewRouter(routes map[string]string, allowPassthrough bool) http.Handler {
	// Pre-build one reverse proxy per configured route.
	handlers := make(map[string]http.Handler, len(routes))
	for host, backend := range routes {
		h, err := NewReverseProxy(backend)
		if err != nil {
			slog.Error("invalid backend URL", "host", host, "backend", backend, "error", err)
			continue
		}
		handlers[host] = h
		slog.Info("registered route", "host", host, "backend", backend)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := handlers[r.Host]
		if !ok {
			if !allowPassthrough {
				http.Error(w, "unknown host", http.StatusBadGateway)
				return
			}
			// Passthrough mode: proxy directly to whatever the Host header says.
			h, _ = NewReverseProxy("http://" + r.Host)
		}
		h.ServeHTTP(w, r)
	})
}

// =============================================================================
// CONCEPT 13: Reverse proxy
// Source: proxy/reverse_proxy.go:43-98  (NewReverseProxy)
//         proxy/reverse_proxy.go:35-39  (shared transport)
// =============================================================================
//
// A reverse proxy receives a request from a client and forwards it to a backend
// server, then relays the response back. The client never speaks to the backend
// directly.
//
// Go's httputil.ReverseProxy does the heavy lifting. We customise two things:
//
//   Director   — mutates the outgoing request before it is sent.
//                We rewrite the Host header to match the backend, not the gateway.
//   Transport  — the shared connection pool (see below).
//
// Shared transport: each backend reuses the same *http.Transport. This means TCP
// connections are pooled across all requests to the same backend. Without this,
// every proxied request opens a new TCP connection — expensive at scale.

var sharedTransport = &http.Transport{
	MaxIdleConns:        200, // total idle connections across all backends
	MaxIdleConnsPerHost: 20,  // idle connections per individual backend
	IdleConnTimeout:     90 * time.Second,
}

// statusWriter captures the status code the backend returned.
// The circuit breaker uses this to detect 5xx responses.
type statusWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func (sw *statusWriter) WriteHeader(status int) {
	sw.status = status
	sw.written = true
	sw.ResponseWriter.WriteHeader(status)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if !sw.written {
		sw.status = http.StatusOK
	}
	return sw.ResponseWriter.Write(b)
}

// NewReverseProxy creates a proxying handler with a circuit breaker attached.
func NewReverseProxy(backend string) (http.Handler, error) {
	target, err := url.Parse(backend)
	if err != nil {
		return nil, err
	}

	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = sharedTransport

	// Overwrite the Host header — without this, the backend sees the gateway's Host.
	original := rp.Director
	rp.Director = func(req *http.Request) {
		original(req)
		req.Host = target.Host
	}

	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Error("backend connection error", "backend", backend, "error", err)
		http.Error(w, "backend unavailable", http.StatusBadGateway)
	}

	// Wrap with a circuit breaker (see Concept 14).
	cb := newCircuitBreaker(backend)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		_, err := cb.Execute(func() (interface{}, error) {
			rp.ServeHTTP(sw, r)
			// Tell the circuit breaker about 5xx responses.
			if sw.status >= 500 {
				return nil, fmt.Errorf("backend returned %d", sw.status)
			}
			return nil, nil
		})

		if err == gobreaker.ErrOpenState || err == gobreaker.ErrTooManyRequests {
			// Circuit is open — the Execute call returned immediately without proxying.
			http.Error(w, "service temporarily unavailable", http.StatusServiceUnavailable)
		}
	}), nil
}

// =============================================================================
// CONCEPT 14: Circuit breaker pattern
// Source: proxy/reverse_proxy.go:64-79  (gobreaker.Settings)
//         proxy/reverse_proxy.go:81-98  (cb.Execute wrapping the proxy call)
// =============================================================================
//
// Problem: if a backend is down and we keep forwarding requests, each one hangs
// until its timeout, exhausting goroutines and file descriptors.
//
// Solution: the circuit breaker tracks failures and "opens" after a threshold,
// immediately rejecting requests (fail fast) instead of waiting.
//
// Three states:
//
//   CLOSED  — normal operation; failures are counted
//   OPEN    — backend is considered down; all requests fail immediately (503)
//   HALF-OPEN — after Timeout, one probe request is allowed through to test recovery
//
//   CLOSED ──(5 consecutive failures)──► OPEN
//     ▲                                    │
//     │                                    │ (30 seconds)
//     └─────(probe succeeds)────── HALF-OPEN
//
// gobreaker.Execute wraps the actual work. If the circuit is open it returns
// ErrOpenState without calling the function at all.

func newCircuitBreaker(name string) *gobreaker.CircuitBreaker {
	return gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        name,
		MaxRequests: 5,            // max concurrent requests in HALF-OPEN state
		Interval:    60 * time.Second, // reset failure counter after this window
		Timeout:     30 * time.Second, // how long to stay OPEN before trying HALF-OPEN
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			// Open the circuit after 5 consecutive failures.
			return counts.ConsecutiveFailures >= 5
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			slog.Warn("circuit breaker state changed",
				"backend", name,
				"from", from.String(),
				"to", to.String(),
			)
		},
	})
}

// =============================================================================
// CONCEPT 15: Middleware chain composition
// Source: cmd/main.go:40-56
// =============================================================================
//
// Each middleware wraps the next, forming a nested call stack.
// The ORDER matters:
//
//   Recovery     — outermost: catches panics from everything below
//   RequestID    — early: injects trace ID so all subsequent logs can use it
//   SecurityHeaders — set on every response regardless of outcome
//   Logger       — wraps everything to measure true end-to-end duration & status
//   Metrics      — same as logger but for Prometheus
//   Timeout      — must be before rate limiting and proxying so they honour it
//   MaxBodySize  — before the body is read by rate limiter or proxy
//   RateLimit    — before the expensive proxy operation
//   Router/Proxy — innermost: the actual work

func buildChain(rl *RateLimiterClient, routes map[string]string, allowPassthrough bool) http.Handler {
	return Recovery(
		RequestID(
			SecurityHeaders(
				Logger(
					Metrics(
						Timeout(30*time.Second,
							MaxBodySize(10<<20, // 10 MB
								RateLimit(rl, routes,
									NewRouter(routes, allowPassthrough),
								),
							),
						),
					),
				),
			),
		),
	)
}

// =============================================================================
// CONCEPT 16: Health checks — liveness and readiness probes
// Source: cmd/main.go:110-115  (livezHandler)
//         cmd/main.go:118-128  (readyzHandler)
// =============================================================================
//
// Kubernetes (and other orchestrators) use two probes:
//
//   Liveness  (/livez)  — "is the process alive?"
//                         Fails → restart the container.
//                         Should be trivial — just return 200.
//
//   Readiness (/readyz) — "is the service ready to accept traffic?"
//                         Fails → remove from the load balancer.
//                         Should check external dependencies.
//
// Key distinction: a slow dependency should make the pod NOT READY (readiness)
// but should NOT trigger a restart (liveness). Only return 503 on readiness
// when you cannot serve traffic; never make liveness check dependencies.

func livezHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always 200 if the process is alive. No dependency checks.
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
}

func readyzHandler(rl *RateLimiterClient) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := rl.Ping(); err != nil {
			// We cannot rate-limit requests — tell the load balancer to route elsewhere.
			http.Error(w, "rate limiter unreachable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ready"))
	})
}

// =============================================================================
// CONCEPT 17: Graceful shutdown
// Source: cmd/main.go:95-107
// =============================================================================
//
// When the OS sends SIGTERM (e.g. during a deploy), we should:
//   1. Stop accepting new connections
//   2. Wait for in-flight requests to complete (up to a deadline)
//   3. Exit cleanly
//
// srv.Shutdown(ctx) does exactly this. Requests that are still in progress get
// to finish. New requests are rejected. After 10 seconds we force-quit.
//
// The signal.Notify channel blocks main() until a signal arrives,
// at which point we initiate the shutdown sequence.

func runWithGracefulShutdown(srv *http.Server) {
	// Start serving in a background goroutine so main() can block on the signal.
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Block until SIGINT (Ctrl+C) or SIGTERM (kill / kubectl rollout).
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down server...")

	// Give in-flight requests 10 seconds to finish.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("forced shutdown", "error", err)
	}
	slog.Info("server stopped")
}

// =============================================================================
// CONCEPT 18: TLS and HTTP → HTTPS redirect
// Source: cmd/main.go:75-91
// =============================================================================
//
// If both TLS_CERT_FILE and TLS_KEY_FILE are set, the gateway serves HTTPS.
// A separate goroutine listens on :80 and redirects all HTTP requests to HTTPS.
//
// http.StatusMovedPermanently (301) tells browsers to always use HTTPS in future.
// r.Host + r.RequestURI reconstructs the full URL, preserving path and query string.

func startTLS(srv *http.Server, certFile, keyFile string) {
	// Redirect HTTP → HTTPS.
	go http.ListenAndServe(":80", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := "https://" + r.Host + r.RequestURI
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	}))

	// Serve HTTPS on the configured port (usually 443 or 8443).
	if err := srv.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
		slog.Error("TLS server error", "error", err)
		os.Exit(1)
	}
}

// =============================================================================
// CONCEPT 19: Prometheus metrics endpoint with optional bearer token auth
// Source: cmd/main.go:131-143
// =============================================================================
//
// promhttp.Handler() serves the default Prometheus registry at /metrics.
// It outputs text in the Prometheus exposition format that Prometheus scrapes.
//
// To prevent exposing internal metrics publicly, an optional bearer token
// can protect the endpoint. If METRICS_TOKEN is empty, the endpoint is open.

func metricsHandler(token string) http.Handler {
	h := promhttp.Handler() // the standard Prometheus scrape endpoint
	if token == "" {
		return h // no auth required
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Validate "Authorization: Bearer <token>" header.
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// =============================================================================
// CONCEPT 20: Configuration via environment variables (12-factor app)
// Source: config/config.go:34-58  (Load func)
//         config/config.go:83-118 (Validate func)
//         config/config.go:125-139 (getEnv / getEnvInt helpers)
// =============================================================================
//
// The 12-factor app methodology says configuration should come from the
// environment, not from files baked into the image. This means:
//
//   - The same Docker image runs in dev, staging, and production.
//   - Configuration changes without a rebuild.
//   - No secrets in source control.
//
// os.Getenv reads an env var; if empty, the default value is used.
// All config is loaded once at startup and passed to handlers as plain values.

type Config struct {
	Port                  string
	RateLimiterURL        string
	RoutesFile            string
	AllowPassthrough      bool
	MetricsToken          string
	MaxBodyBytes          int64
	RequestTimeout        time.Duration
	TLSCertFile           string
	TLSKeyFile            string
	LogLevel              string
	Routes                map[string]string
}

func LoadConfig() Config {
	maxBodyMB, _ := strconv.ParseInt(getEnv("MAX_BODY_MB", "10"), 10, 64)
	timeoutSec, _ := strconv.ParseInt(getEnv("REQUEST_TIMEOUT_SECONDS", "30"), 10, 64)

	return Config{
		Port:           getEnv("PORT", "8080"),
		RateLimiterURL: getEnv("RATE_LIMITER_URL", "http://localhost:9090"),
		AllowPassthrough: getEnv("ALLOW_PASSTHROUGH", "false") == "true",
		MetricsToken:   getEnv("METRICS_TOKEN", ""),
		MaxBodyBytes:   maxBodyMB << 20, // convert MB to bytes
		RequestTimeout: time.Duration(timeoutSec) * time.Second,
		TLSCertFile:    getEnv("TLS_CERT_FILE", ""),
		TLSKeyFile:     getEnv("TLS_KEY_FILE", ""),
		LogLevel:       getEnv("LOG_LEVEL", "info"),
	}
}

func (c Config) TLSEnabled() bool {
	return c.TLSCertFile != "" && c.TLSKeyFile != ""
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// =============================================================================
// PUTTING IT ALL TOGETHER
// =============================================================================
//
// This is a sketch of the full main() that wires every concept together.
// See cmd/main.go for the real implementation.
//
//   func main() {
//       cfg := LoadConfig()
//       rl  := NewRateLimiterClient(cfg.RateLimiterURL)
//
//       handler := buildChain(rl, cfg.Routes, cfg.AllowPassthrough)
//
//       mux := http.NewServeMux()
//       mux.Handle("/livez",   livezHandler())
//       mux.Handle("/readyz",  readyzHandler(rl))
//       mux.Handle("/healthz", livezHandler())
//       mux.Handle("/metrics", metricsHandler(cfg.MetricsToken))
//       mux.Handle("/",        handler)
//
//       srv := &http.Server{
//           Addr:         ":" + cfg.Port,
//           Handler:      mux,
//           ReadTimeout:  10 * time.Second,
//           WriteTimeout: 30 * time.Second,
//           IdleTimeout:  60 * time.Second,
//       }
//
//       if cfg.TLSEnabled() {
//           go startTLS(srv, cfg.TLSCertFile, cfg.TLSKeyFile)
//       }
//       runWithGracefulShutdown(srv)
//   }
//
// Request flow summary:
//
//   Client
//     │
//     ▼
//   Recovery          ← catches panics, returns 500
//     │
//   RequestID         ← injects/reads X-Request-ID, attaches to context
//     │
//   SecurityHeaders   ← sets X-Frame-Options, CSP, etc.
//     │
//   Logger            ← logs method, host, path, status, duration after response
//     │
//   Metrics           ← increments Prometheus counters/histograms after response
//     │
//   Timeout           ← cancels context after 30s
//     │
//   MaxBodySize       ← wraps body reader with 10 MB cap
//     │
//   RateLimit         ← checks external rate limiter; fail-open; 429 if denied
//     │
//   Router            ← matches Host header → backend handler
//     │
//   ReverseProxy      ← forwards to backend; circuit breaker wraps the call
//     │
//   Backend Service
