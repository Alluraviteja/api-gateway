package router

import (
	"log/slog"
	"net/http"

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
			slog.Warn("unknown host rejected", "host", r.Host)
			http.Error(w, "unknown host", http.StatusBadGateway)
			return
		}
		h.ServeHTTP(w, r)
	})
}
