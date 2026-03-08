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
	ClientID string `json:"clientId"`
	AppID    string `json:"appId"`
	Endpoint string `json:"endpoint"`
}

type checkResponse struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
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
func (c *RateLimiterClient) IsAllowed(clientID, appID, endpoint string) (bool, error) {
	payload := checkRequest{
		ClientID: clientID,
		AppID:    appID,
		Endpoint: endpoint,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("marshal rate limit request: %w", err)
	}

	resp, err := c.httpClient.Post(
		c.baseURL+"/rate-limit/check",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		// Fail open: allow request if rate limiter is unreachable.
		return true, fmt.Errorf("rate limiter unreachable: %w", err)
	}
	defer resp.Body.Close()

	var result checkResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return true, fmt.Errorf("decode rate limit response: %w", err)
	}

	return result.Allowed, nil
}

// Ping checks whether the Rate Limiter Service is reachable.
// Any HTTP response (including 4xx) means the server is up.
func (c *RateLimiterClient) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.baseURL, nil)
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
