package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"api-gateway/client"
	"api-gateway/config"
	"api-gateway/middleware"
)

func main() {
	// Bootstrap with info-level logger until config is loaded.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg := config.Load()

	if err := cfg.Validate(); err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	// Re-initialise logger with configured level.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(cfg.LogLevel),
	})))

	rl := client.NewRateLimiterClient(cfg.RateLimiterURL)

	// Build handler chain (outermost → innermost):
	// Recovery → RequestID → SecurityHeaders → Logger → Metrics → Timeout → MaxBodySize → RateLimit
	handler := middleware.Recovery(
		middleware.RequestID(
			middleware.SecurityHeaders(
				middleware.Logger(
					middleware.Metrics(
						middleware.Timeout(cfg.RequestTimeout,
							middleware.MaxBodySize(cfg.MaxBodyBytes,
								middleware.RateLimit(rl),
							),
						),
					),
				),
			),
		),
	)

	mux := http.NewServeMux()
	mux.Handle("/livez", livezHandler())
	mux.Handle("/readyz", readyzHandler(rl))
	mux.Handle("/healthz", livezHandler()) // alias for backward compatibility
	mux.Handle("/api/v1/ping", pingHandler(rl))
	mux.Handle("/metrics", metricsHandler(cfg.MetricsToken))
	mux.Handle("/", handler)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Bind the port before starting so we can log a confirmed "started" message.
	ln, err := net.Listen("tcp", ":"+cfg.Port)
	if err != nil {
		slog.Error("failed to bind port", "port", cfg.Port, "error", err)
		os.Exit(1)
	}

	// Start server in background.
	go func() {
		if cfg.TLSEnabled() {
			slog.Info("API Gateway starting with TLS", "port", cfg.Port)
			// Redirect HTTP → HTTPS on port 80.
			go http.ListenAndServe(":80", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "https://"+r.Host+r.RequestURI, http.StatusMovedPermanently)
			}))
			if err := srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile); err != nil && err != http.ErrServerClosed {
				slog.Error("server error", "error", err)
				os.Exit(1)
			}
		} else {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				slog.Error("server error", "error", err)
				os.Exit(1)
			}
		}
	}()
	slog.Info("API Gateway started", "port", cfg.Port)

	// Graceful shutdown on SIGINT/SIGTERM.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("forced shutdown", "error", err)
	}
	slog.Info("server stopped")
}

// livezHandler confirms the process is alive.
func livezHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
}

// readyzHandler confirms the gateway and its dependencies are ready to serve traffic.
func readyzHandler(rl *client.RateLimiterClient) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := rl.Ping(); err != nil {
			slog.Warn("readiness check failed", "error", err)
			http.Error(w, "rate limiter unreachable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ready"))
	})
}

// pingHandler reports the gateway status and whether the Rate Limiter Service is reachable.
func pingHandler(rl *client.RateLimiterClient) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rateLimiterStatus := "up"
		if err := rl.Ping(); err != nil {
			slog.Warn("ping: rate limiter unreachable", "error", err)
			rateLimiterStatus = "down"
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"gateway":     "up",
			"rateLimiter": rateLimiterStatus,
		})
	})
}

// metricsHandler protects the Prometheus metrics endpoint with an optional bearer token.
func metricsHandler(token string) http.Handler {
	h := promhttp.Handler()
	if token == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// logLevel converts a string log level to slog.Level.
func logLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
