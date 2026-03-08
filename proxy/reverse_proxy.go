package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/sony/gobreaker"
)

// statusWriter captures the HTTP status code written by the reverse proxy.
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

// transport is a shared, tuned HTTP transport for all backend proxies.
var transport = &http.Transport{
	MaxIdleConns:        200,
	MaxIdleConnsPerHost: 20,
	IdleConnTimeout:     90 * time.Second,
}

// NewReverseProxy returns an HTTP handler that proxies requests to the given backend URL.
// Includes a circuit breaker that opens after 5 consecutive backend failures.
func NewReverseProxy(backend string) (http.Handler, error) {
	target, err := url.Parse(backend)
	if err != nil {
		return nil, err
	}

	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = transport

	// Rewrite the Host header to match the backend.
	original := rp.Director
	rp.Director = func(req *http.Request) {
		original(req)
		req.Host = target.Host
	}

	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Error("backend connection error", "backend", backend, "error", err)
		http.Error(w, "backend unavailable", http.StatusBadGateway)
	}

	cb := gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        backend,
		MaxRequests: 5,
		Interval:    60 * time.Second,
		Timeout:     30 * time.Second,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
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

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		_, err := cb.Execute(func() (interface{}, error) {
			rp.ServeHTTP(sw, r)
			if sw.status >= 500 {
				return nil, fmt.Errorf("backend returned %d", sw.status)
			}
			return nil, nil
		})

		// Circuit is open — Execute returned without calling our func, nothing written yet.
		if err == gobreaker.ErrOpenState || err == gobreaker.ErrTooManyRequests {
			slog.Warn("circuit open, rejecting request", "backend", backend)
			http.Error(w, "service temporarily unavailable", http.StatusServiceUnavailable)
		}
	}), nil
}
