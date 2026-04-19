package middleware

import (
	"fmt"
	"log/slog"
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
// If the response contains a serviceUrl, the request is proxied directly to it.
// If not, next is called (passthrough / router fallback).
func RateLimit(rl *client.RateLimiterClient, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := r.Header.Get("X-Client-ID")
		if clientIP == "" {
			clientIP = remoteIP(r)
		}

		serviceName := resolveServiceName(r)

		result, err := rl.IsAllowed(serviceName, clientIP, r.URL.Path, r.Method)
		if err != nil {
			slog.Warn("rate limiter error", "client", clientIP, "error", err)
		}

		if !result.Allowed {
			slog.Info("request rate limited", "client", clientIP, "host", r.Host, "path", r.URL.Path)
			if result.RetryAfterSecs > 0 {
				w.Header().Set("Retry-After", fmt.Sprintf("%d", result.RetryAfterSecs))
			}
			http.Error(w, "429 Too Many Requests", http.StatusTooManyRequests)
			return
		}

		if result.ServiceURL != "" {
			h, err := getOrCreateProxy(result.ServiceURL)
			if err != nil {
				slog.Error("failed to create proxy", "serviceUrl", result.ServiceURL, "error", err)
				http.Error(w, "bad gateway", http.StatusBadGateway)
				return
			}
			h.ServeHTTP(w, r)
			return
		}

		// No serviceUrl returned (e.g. no app registered) — fall through to router.
		next.ServeHTTP(w, r)
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

// resolveServiceName determines the service name from the request.
// Priority:
//  1. Subdomain from Host header (e.g. "personal-website.example.com" -> "personal-website")
//  2. X-Service-Name header (e.g. when calling via IP like server_ip:8080)
func resolveServiceName(r *http.Request) string {
	host := r.Host
	// Strip port if present.
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}
	// Use subdomain if host has 3+ parts (subdomain.domain.tld).
	parts := strings.Split(host, ".")
	if len(parts) >= 3 {
		return parts[0]
	}
	// Fall back to X-Service-Name header.
	return r.Header.Get("X-Service-Name")
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
