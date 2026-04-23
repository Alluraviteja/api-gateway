package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	"api-gateway/router"
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
	r := router.New(cfg.Routes)

	// Build handler chain (outermost → innermost):
	// Recovery → RequestID → SecurityHeaders → Logger → Metrics → Timeout → MaxBodySize → RateLimit → Router
	handler := middleware.Recovery(
		middleware.RequestID(
			middleware.SecurityHeaders(
				middleware.Logger(
					middleware.Metrics(
						middleware.Timeout(cfg.RequestTimeout,
							middleware.MaxBodySize(cfg.MaxBodyBytes,
								middleware.RateLimit(rl, cfg.Routes, r),
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
	mux.Handle("/api/v1/ratelimit/check", rateLimitCheckHandler(rl))
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

// rateLimitCheckHandler exposes POST /api/v1/ratelimit/check on the gateway.
// It forwards the check to the Rate Limiter Service and mirrors its response.
// traceId and Authorization are optional.
func rateLimitCheckHandler(rl *client.RateLimiterClient) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			ServiceIdentifier string `json:"serviceIdentifier"`
			ClientIP          string `json:"clientIp"`
			RequestPath       string `json:"requestPath"`
			HTTPMethod        string `json:"httpMethod"`
			TraceID           string `json:"traceId"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		if req.ServiceIdentifier == "" || req.ClientIP == "" || req.RequestPath == "" || req.HTTPMethod == "" {
			http.Error(w, "serviceIdentifier, clientIp, requestPath, and httpMethod are required", http.StatusBadRequest)
			return
		}

		result, err := rl.IsAllowed(req.ServiceIdentifier, req.ClientIP, req.RequestPath, req.HTTPMethod, req.TraceID)
		if err != nil {
			slog.Error("rate limit check error", "error", err, "traceId", req.TraceID)
		}

		w.Header().Set("Content-Type", "application/json")

		if !result.Allowed {
			if result.RetryAfterSecs > 0 {
				w.Header().Set("Retry-After", fmt.Sprintf("%d", result.RetryAfterSecs))
			}
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]any{
				"allowed":           false,
				"retryAfterSeconds": result.RetryAfterSecs,
			})
			return
		}

		json.NewEncoder(w).Encode(map[string]any{
			"allowed": true,
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
