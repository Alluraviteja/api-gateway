package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// responseWriter wraps http.ResponseWriter to capture status code and response size.
type responseWriter struct {
	http.ResponseWriter
	status  int
	size    int
	written bool
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.written = true
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.written {
		rw.status = http.StatusOK
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.size += n
	return n, err
}

// Logger logs a structured access log line for every request.
func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rw, r)

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
