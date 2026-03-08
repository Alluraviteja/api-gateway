package middleware

import (
	"context"
	"net/http"
	"time"
)

// Timeout cancels the request context after the given duration.
// Prevents slow or hung backends from holding connections open indefinitely.
func Timeout(duration time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), duration)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
