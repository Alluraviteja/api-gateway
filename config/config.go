package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"
)

// Config holds all gateway configuration loaded from environment variables.
type Config struct {
	Port             string
	LogLevel         string
	RateLimiterURL   string
	Routes           map[string]string // host -> backend URL
	AllowPassthrough bool
	TLSCertFile      string
	TLSKeyFile       string
	MetricsToken     string
	MaxBodyBytes     int64
	RequestTimeout   time.Duration
}

// Load reads configuration from environment variables with sensible defaults.
func Load() *Config {
	return &Config{
		Port:             getEnv("PORT", "8080"),
		LogLevel:         getEnv("LOG_LEVEL", "info"),
		RateLimiterURL:   getEnv("RATE_LIMITER_URL", "http://localhost:9090"),
		Routes: map[string]string{
			getEnv("APP1_HOST", "app1.test.com"): getEnv("APP1_BACKEND", "http://app1-service"),
			getEnv("APP2_HOST", "app2.test.com"): getEnv("APP2_BACKEND", "http://app2-service"),
		},
		AllowPassthrough: getEnv("ALLOW_PASSTHROUGH", "false") == "true",
		TLSCertFile:      os.Getenv("TLS_CERT_FILE"),
		TLSKeyFile:       os.Getenv("TLS_KEY_FILE"),
		MetricsToken:     os.Getenv("METRICS_TOKEN"),
		MaxBodyBytes:     int64(getEnvInt("MAX_BODY_MB", 10)) * 1024 * 1024,
		RequestTimeout:   time.Duration(getEnvInt("REQUEST_TIMEOUT_SECONDS", 30)) * time.Second,
	}
}

// Validate checks that required configuration values are present and valid.
func (c *Config) Validate() error {
	if c.Port == "" {
		return fmt.Errorf("PORT must not be empty")
	}

	if _, err := url.ParseRequestURI(c.RateLimiterURL); err != nil {
		return fmt.Errorf("invalid RATE_LIMITER_URL %q: %w", c.RateLimiterURL, err)
	}

	for host, backend := range c.Routes {
		if host == "" {
			return fmt.Errorf("route has empty host")
		}
		if _, err := url.ParseRequestURI(backend); err != nil {
			return fmt.Errorf("invalid backend URL for host %q: %w", host, err)
		}
	}

	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return fmt.Errorf("TLS_CERT_FILE and TLS_KEY_FILE must both be set or both be empty")
	}

	if c.MaxBodyBytes <= 0 {
		return fmt.Errorf("MAX_BODY_MB must be greater than 0")
	}

	if c.RequestTimeout <= 0 {
		return fmt.Errorf("REQUEST_TIMEOUT_SECONDS must be greater than 0")
	}

	return nil
}

// TLSEnabled returns true if TLS is configured.
func (c *Config) TLSEnabled() bool {
	return c.TLSCertFile != "" && c.TLSKeyFile != ""
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return defaultVal
}
