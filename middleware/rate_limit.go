package middleware

import (
	"log/slog"
	"net/http"
	"strings"

	"api-gateway/client"
)

// RateLimit checks the Rate Limiter Service before forwarding the request.
// Hosts not present in routes are passed through without rate limiting.
func RateLimit(rl *client.RateLimiterClient, routes map[string]string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If host is not a configured route, skip rate limiting entirely.
		if _, ok := routes[r.Host]; !ok {
			slog.Info("unknown host, passing through without rate limit", "host", r.Host)
			next.ServeHTTP(w, r)
			return
		}

		clientID := r.Header.Get("X-Client-ID")
		if clientID == "" {
			clientID = remoteIP(r)
		}

		appID := appIDFromHost(r.Host)

		allowed, err := rl.IsAllowed(clientID, appID, r.URL.Path)
		if err != nil {
			slog.Warn("rate limiter error", "client", clientID, "error", err)
		}

		if !allowed {
			slog.Info("request rate limited", "client", clientID, "host", r.Host, "path", r.URL.Path)
			http.Error(w, "429 Too Many Requests", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// appIDFromHost extracts the app identifier from the host header.
// e.g. "app1.test.com" -> "app1"
func appIDFromHost(host string) string {
	parts := strings.SplitN(host, ".", 2)
	return parts[0]
}

// remoteIP extracts the client IP, respecting X-Forwarded-For.
func remoteIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return strings.SplitN(ip, ",", 2)[0]
	}
	if idx := strings.LastIndex(r.RemoteAddr, ":"); idx != -1 {
		return r.RemoteAddr[:idx]
	}
	return r.RemoteAddr
}
