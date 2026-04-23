package middleware

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"api-gateway/client"
)

// RateLimit checks the Rate Limiter Service before passing to next.
// Skips the rate limiter if the request host is not in the routes map.
// If the request is rate limited, it responds with 429 and stops.
func RateLimit(rl *client.RateLimiterClient, routes map[string]string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := routes[r.Host]; !ok {
			slog.Warn("host not in routes, rejecting", "host", r.Host)
			http.Error(w, "unknown host", http.StatusBadGateway)
			return
		}

		clientIP := r.Header.Get("X-Client-ID")
		if clientIP == "" {
			clientIP = remoteIP(r)
		}

		serviceIdentifier := resolveServiceIdentifier(r)

		result, err := rl.IsAllowed(serviceIdentifier, clientIP, r.URL.Path, r.Method, "")
		if err != nil {
			slog.Error("rate limiter error",
				"error", err,
				"serviceIdentifier", serviceIdentifier,
				"clientIp", clientIP,
				"requestPath", r.URL.Path,
				"httpMethod", r.Method,
			)
		}

		if !result.Allowed {
			slog.Warn("request rate limited",
				"serviceIdentifier", serviceIdentifier,
				"clientIp", clientIP,
				"requestPath", r.URL.Path,
				"httpMethod", r.Method,
			)
			w.Header().Set("RateLimit-Limit", fmt.Sprintf("%d", result.Limit))
			w.Header().Set("RateLimit-Remaining", "0")
			w.Header().Set("RateLimit-Reset", fmt.Sprintf("%d", result.ResetAfterSeconds))
			w.Header().Set("Retry-After", fmt.Sprintf("%d", result.RetryAfterSecs))
			http.Error(w, "429 Too Many Requests", http.StatusTooManyRequests)
			return
		}

		if result.Limit > 0 {
			w.Header().Set("RateLimit-Limit", fmt.Sprintf("%d", result.Limit))
			w.Header().Set("RateLimit-Remaining", fmt.Sprintf("%d", result.Remaining))
			w.Header().Set("RateLimit-Reset", fmt.Sprintf("%d", result.ResetAfterSeconds))
		}

		next.ServeHTTP(w, r)
	})
}

// resolveServiceIdentifier returns a single value to identify the service:
//   - Subdomain from Host header (e.g. "personal-website.example.com" → "personal-website")
//   - Port from Host header if request is to an IP (e.g. "5.78.139.110:8085" → "8085")
func resolveServiceIdentifier(r *http.Request) string {
	host := r.Host
	port := ""
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		port = host[idx+1:]
		host = host[:idx]
	}
	if net.ParseIP(host) != nil {
		return port
	}
	parts := strings.Split(host, ".")
	if len(parts) >= 3 {
		return parts[0]
	}
	return ""
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
