package router

import (
	"log/slog"
	"net"
	"net/http"
	"strings"

	"api-gateway/proxy"
)

// New builds an HTTP handler that routes requests to backends based on the Host header.
// Unknown hosts receive 502 Bad Gateway — use routes.yaml to register all valid hosts.
func New(routes map[string]string) http.Handler {
	handlers := make(map[string]http.Handler, len(routes))

	for host, backend := range routes {
		h, err := proxy.NewReverseProxy(backend)
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
			h, ok = handlers[hostKey(r.Host)]
		}
		if !ok {
			slog.Warn("unknown host rejected", "host", r.Host)
			http.Error(w, "unknown host", http.StatusBadGateway)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// hostKey extracts a short key from the Host header for route lookup:
//   - IP host (e.g. "5.78.139.110:8090") → port ("8090")
//   - Named host with subdomain (e.g. "app.example.com") → subdomain ("app")
//   - Anything else → empty string (no match)
func hostKey(host string) string {
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		h = host
		port = ""
	}
	if net.ParseIP(h) != nil {
		return port
	}
	parts := strings.Split(h, ".")
	if len(parts) >= 3 {
		return parts[0]
	}
	return ""
}
