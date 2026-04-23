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
	Allowed           bool
	Limit             int
	Remaining         int
	ResetAfterSeconds int
	RetryAfterSecs    int
}

type checkResponse struct {
	Allowed           bool   `json:"allowed"`
	Limit             int    `json:"limit"`
	RemainingTokens   int    `json:"remainingTokens"`
	ResetAfterSeconds int    `json:"resetAfterSeconds"`
	ServiceName       string `json:"serviceName"`
	ServiceURL        string `json:"serviceUrl"`
	MatchedPattern    string `json:"matchedPattern"`
	Timestamp         string `json:"timestamp"`
}

type errorResponse struct {
	Error             string `json:"error"`
	Code              int    `json:"code"`
	Message           string `json:"message"`
	RetryAfterSeconds int    `json:"retryAfterSeconds"`
	Timestamp         string `json:"timestamp"`
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
			Allowed:           result.Allowed,
			Limit:             result.Limit,
			Remaining:         result.RemainingTokens,
			ResetAfterSeconds: result.ResetAfterSeconds,
		}, nil

	case http.StatusTooManyRequests:
		var errResp errorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
			return CheckResult{Allowed: false}, fmt.Errorf("decode 429 response: %w", err)
		}
		return CheckResult{
			Allowed:           false,
			Limit:             0,
			Remaining:         0,
			ResetAfterSeconds: errResp.RetryAfterSeconds,
			RetryAfterSecs:    errResp.RetryAfterSeconds,
		}, nil

	case http.StatusNotFound:
		var errResp errorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
			return CheckResult{Allowed: true}, fmt.Errorf("decode 404 response: %w", err)
		}
		return CheckResult{Allowed: true}, fmt.Errorf("service not registered in rate limiter: %s", errResp.Message)

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
