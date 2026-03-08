package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_requests_total",
		Help: "Total number of requests processed by the gateway.",
	}, []string{"host", "method", "status"})

	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_request_duration_seconds",
		Help:    "Request duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"host", "method"})

	rateLimitedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_rate_limited_total",
		Help: "Total number of requests rejected by rate limiting.",
	}, []string{"host"})
)

// Metrics records Prometheus metrics for every request.
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
