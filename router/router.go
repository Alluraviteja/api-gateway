package router

import (
	"log/slog"
	"net/http"

	"api-gateway/proxy"
)

// New builds an HTTP handler that routes requests to backends based on the Host header.
// If allowPassthrough is true, unknown hosts are proxied directly to their origin.
// If false, unknown hosts receive 502 Bad Gateway.
func New(routes map[string]string, allowPassthrough bool) http.Handler {
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
			if !allowPassthrough {
				slog.Warn("unknown host rejected", "host", r.Host)
				http.Error(w, "unknown host", http.StatusBadGateway)
				return
			}

			// Pass-through: proxy directly to the host as-is.
			passthrough, err := proxy.NewReverseProxy("http://" + r.Host)
			if err != nil {
				http.Error(w, "bad gateway", http.StatusBadGateway)
				return
			}
			slog.Info("passing through unconfigured host", "host", r.Host)
			passthrough.ServeHTTP(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}
