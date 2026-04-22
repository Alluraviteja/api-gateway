package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// RateLimiterClient calls the Rate Limiter Service.
type RateLimiterClient struct {
	baseURL    string
	httpClient *http.Client
}

type checkRequest struct {
	ServiceIdentifier string `json:"serviceIdentifier"`
	ClientIP          string `json:"clientIp"`
	RequestPath       string `json:"requestPath"`
	HTTPMethod        string `json:"httpMethod"`
	TraceID           string `json:"traceId,omitempty"`
}

// CheckResult holds the parsed response from the Rate Limiter Service.
type CheckResult struct {
	Allowed        bool
	ServiceURL     string
	RetryAfterSecs int
}

type checkResponse struct {
	ServiceName     string `json:"serviceName"`
	ServiceURL      string `json:"serviceUrl"`
	Allowed         bool   `json:"allowed"`
	RemainingTokens int    `json:"remainingTokens"`
	Reason          string `json:"reason"`
}

type errorResponse struct {
	Status             string `json:"status"`
	Code               int    `json:"code"`
	Message            string `json:"message"`
	RetryAfterSeconds  int    `json:"retryAfterSeconds"`
}

// NewRateLimiterClient creates a new client for the Rate Limiter Service.
func NewRateLimiterClient(baseURL string) *RateLimiterClient {
	return &RateLimiterClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 2 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// IsAllowed checks with the Rate Limiter Service whether the request is permitted.
// Returns a CheckResult containing allowed status, serviceUrl, and retryAfter seconds.
// Fail-open: if the service is unreachable, allowed=true is returned.
// traceId is optional; pass "" to omit it.
func (c *RateLimiterClient) IsAllowed(serviceIdentifier, clientIP, requestPath, httpMethod, traceID string) (CheckResult, error) {
	payload := checkRequest{
		ServiceIdentifier: serviceIdentifier,
		ClientIP:          clientIP,
		RequestPath:       requestPath,
		HTTPMethod:        httpMethod,
		TraceID:           traceID,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return CheckResult{Allowed: true}, fmt.Errorf("marshal rate limit request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/v1/ratelimit/check", bytes.NewReader(body))
	if err != nil {
		return CheckResult{Allowed: true}, fmt.Errorf("build rate limit request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return CheckResult{Allowed: true}, fmt.Errorf("rate limiter unreachable: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var result checkResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return CheckResult{Allowed: true}, fmt.Errorf("decode rate limit response: %w", err)
		}
		return CheckResult{
			Allowed:    result.Allowed,
			ServiceURL: result.ServiceURL,
		}, nil

	case http.StatusTooManyRequests:
		var errResp errorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
			return CheckResult{Allowed: false}, fmt.Errorf("decode 429 response: %w", err)
		}
		return CheckResult{
			Allowed:        false,
			RetryAfterSecs: errResp.RetryAfterSeconds,
		}, nil

	case http.StatusBadRequest:
		var errResp errorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
			return CheckResult{Allowed: true}, fmt.Errorf("decode 400 response: %w", err)
		}
		return CheckResult{Allowed: true}, fmt.Errorf("bad request to rate limiter: %s", errResp.Message)

	default:
		return CheckResult{Allowed: true}, fmt.Errorf("unexpected rate limiter status: %d", resp.StatusCode)
	}
}

// Ping checks whether the Rate Limiter Service is reachable via its /api/v1/ping endpoint.
// Any HTTP response means the server is up; a connection error means it is down.
func (c *RateLimiterClient) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/ping", nil)
	if err != nil {
		return fmt.Errorf("build ping request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("rate limiter unreachable: %w", err)
	}
	resp.Body.Close()
	return nil
}
