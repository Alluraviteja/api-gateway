package middleware

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"api-gateway/client"
	"api-gateway/proxy"
)

var (
	proxyMu    sync.Mutex
	proxyCache = make(map[string]http.Handler)
)

// RateLimit checks the Rate Limiter Service before forwarding the request.
// The rate limiter response must include a serviceUrl; if absent, 502 is returned.
func RateLimit(rl *client.RateLimiterClient) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := r.Header.Get("X-Client-ID")
		if clientIP == "" {
			clientIP = remoteIP(r)
		}

		serviceIdentifier := resolveServiceIdentifier(r)

		result, err := rl.IsAllowed(serviceIdentifier, clientIP, r.URL.Path, r.Method)
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
			if result.RetryAfterSecs > 0 {
				w.Header().Set("Retry-After", fmt.Sprintf("%d", result.RetryAfterSecs))
			}
			http.Error(w, "429 Too Many Requests", http.StatusTooManyRequests)
			return
		}

		if result.ServiceURL == "" {
			slog.Error("no serviceUrl in rate limiter response",
				"serviceIdentifier", serviceIdentifier,
				"clientIp", clientIP,
				"requestPath", r.URL.Path,
				"httpMethod", r.Method,
			)
			http.Error(w, "service not found", http.StatusBadGateway)
			return
		}

		h, err := getOrCreateProxy(result.ServiceURL)
		if err != nil {
			slog.Error("failed to create proxy",
				"serviceUrl", result.ServiceURL,
				"error", err,
				"serviceIdentifier", serviceIdentifier,
				"clientIp", clientIP,
				"requestPath", r.URL.Path,
				"httpMethod", r.Method,
			)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// getOrCreateProxy returns a cached reverse proxy for the given backend URL.
func getOrCreateProxy(serviceURL string) (http.Handler, error) {
	proxyMu.Lock()
	defer proxyMu.Unlock()
	if h, ok := proxyCache[serviceURL]; ok {
		return h, nil
	}
	h, err := proxy.NewReverseProxy(serviceURL)
	if err != nil {
		return nil, err
	}
	proxyCache[serviceURL] = h
	return h, nil
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
	// IP address — use port as the identifier.
	if net.ParseIP(host) != nil {
		return port
	}
	// Use subdomain if host has 3+ parts (subdomain.domain.tld).
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
