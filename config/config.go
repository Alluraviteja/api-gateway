package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds all gateway configuration loaded from environment variables.
type Config struct {
	Port            string
	LogLevel        string
	RateLimiterURL  string
	TLSCertFile     string
	TLSKeyFile      string
	MetricsToken    string
	MaxBodyBytes    int64
	RequestTimeout  time.Duration
	RoutesFile string
	Routes     map[string]string
}

// Load reads configuration from environment variables with sensible defaults.
func Load() *Config {
	cfg := &Config{
		Port:             getEnv("PORT", "8080"),
		LogLevel:         getEnv("LOG_LEVEL", "info"),
		RateLimiterURL:   getEnv("RATE_LIMITER_URL", "http://localhost:9090"),
		TLSCertFile:      os.Getenv("TLS_CERT_FILE"),
		TLSKeyFile:       os.Getenv("TLS_KEY_FILE"),
		MetricsToken:     os.Getenv("METRICS_TOKEN"),
		MaxBodyBytes:     int64(getEnvInt("MAX_BODY_MB", 10)) * 1024 * 1024,
		RequestTimeout:   time.Duration(getEnvInt("REQUEST_TIMEOUT_SECONDS", 30)) * time.Second,
		RoutesFile: os.Getenv("ROUTES_FILE"),
	}
	cfg.Routes = loadRoutes(cfg.RoutesFile)
	return cfg
}

// Validate checks that required configuration values are present and valid.
func (c *Config) Validate() error {
	if c.Port == "" {
		return fmt.Errorf("PORT must not be empty")
	}

	if _, err := url.ParseRequestURI(c.RateLimiterURL); err != nil {
		return fmt.Errorf("invalid RATE_LIMITER_URL %q: %w", c.RateLimiterURL, err)
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

// loadRoutes returns a host→backend map from a YAML file if ROUTES_FILE is set,
// or falls back to APP1_HOST/APP1_BACKEND … APP9_HOST/APP9_BACKEND env vars.
func loadRoutes(routesFile string) map[string]string {
	if routesFile != "" {
		routes, err := loadRoutesFromFile(routesFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not load routes file %q: %v\n", routesFile, err)
		} else {
			return routes
		}
	}
	return loadRoutesFromEnv()
}

type routesFileSchema struct {
	Routes map[string]string `yaml:"routes"`
}

func loadRoutesFromFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var schema routesFileSchema
	if err := yaml.Unmarshal(data, &schema); err != nil {
		return nil, err
	}
	if schema.Routes == nil {
		return map[string]string{}, nil
	}
	return schema.Routes, nil
}

// loadRoutesFromEnv reads APP1_HOST/APP1_BACKEND … APP9_HOST/APP9_BACKEND pairs.
func loadRoutesFromEnv() map[string]string {
	routes := make(map[string]string)
	for i := 1; i <= 9; i++ {
		host := os.Getenv(fmt.Sprintf("APP%d_HOST", i))
		backend := os.Getenv(fmt.Sprintf("APP%d_BACKEND", i))
		if host != "" && backend != "" {
			routes[host] = backend
		}
	}
	return routes
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
