# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Build
go mod tidy && go build -o bin/api-gateway ./cmd/main.go

# Run locally (Rate Limiter URL required)
RATE_LIMITER_URL=http://localhost:9090 go run ./cmd/main.go

# Lint
go vet ./...

# Test (all tests with race detection and coverage)
go test -v -race -cover ./...

# Run a single test
go test -v -run TestFunctionName ./path/to/package/...

# Docker
docker build -t api-gateway .
docker compose up  # uses .env.local by default
```

## Architecture

This is a **lightweight reverse proxy in Go** that sits in front of multiple backend services. Its two main jobs are:

1. **Route** incoming requests to the correct backend based on the `Host` header
2. **Enforce rate limits** by calling an external Rate Limiter Service before forwarding

### Request pipeline (`cmd/main.go`)

Every request flows through this middleware chain in order:

```
Recovery → RequestID → SecurityHeaders → Logger → Metrics → Timeout → MaxBodySize → RateLimit → Router → ReverseProxy
```

### Key architectural decisions

- **Fail-open rate limiting** (`client/rate_limiter_client.go`): If the Rate Limiter Service is unreachable, the request is allowed through and the error is logged. This is intentional — availability > strict enforcement.
- **Circuit breaker** (`proxy/reverse_proxy.go`): Opens after 5 consecutive 5xx responses from a backend, stays open for 30 seconds. Prevents cascading failures.
- **Passthrough mode** (`router/router.go`): When `ALLOW_PASSTHROUGH=true`, requests to unknown hosts are proxied directly to `http://<host>` without rate limiting. When false, unknown hosts get 502.
- **Rate limit skip for unknown hosts** (`middleware/rate_limit.go`): Rate limiting only applies to hosts that are configured in the routes map.

### Routing configuration

Routes map `Host` header → backend URL. Two configuration methods (first one wins):

1. **YAML file** (preferred for production): set `ROUTES_FILE=/path/to/routes.yaml`
   ```yaml
   routes:
     app1.example.com: http://app1-service
     app2.example.com: http://app2-service
   ```
2. **Environment variables** (for simple setups): `APP1_HOST`, `APP1_BACKEND`, `APP2_HOST`, `APP2_BACKEND`

### Rate limiter integration

Rate limit checks call `POST /rate-limit/check` on the Rate Limiter Service with:
```json
{ "clientId": "<X-Client-ID header or IP>", "appId": "<first segment of Host>", "endpoint": "<path>" }
```

### Key environment variables

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | Gateway listen port |
| `RATE_LIMITER_URL` | `http://localhost:9090` | Rate Limiter Service URL |
| `ROUTES_FILE` | (empty) | Path to YAML routes file |
| `METRICS_TOKEN` | (empty) | Bearer token for `/metrics` (empty = open) |
| `MAX_BODY_MB` | `10` | Max request body in MB |
| `REQUEST_TIMEOUT_SECONDS` | `30` | Per-request timeout |
| `TLS_CERT_FILE` / `TLS_KEY_FILE` | (empty) | Both required to enable HTTPS |

### Health and observability endpoints

- `GET /livez` or `/healthz` — liveness probe
- `GET /readyz` — readiness probe (checks Rate Limiter Service reachability)
- `GET /metrics` — Prometheus metrics (`gateway_requests_total`, `gateway_request_duration_seconds`, `gateway_rate_limited_total`)

## CI/CD

- **`ci.yml`**: Runs on every push/PR — `go vet`, tests, Trivy filesystem scan, Docker image build + scan. Fails on CRITICAL/HIGH vulnerabilities.
- **`build.yml`**: Triggers after CI passes on `main` — builds multi-arch image (amd64 + arm64), pushes to Docker Hub, SCPs `routes.yaml` to server, deploys with automatic rollback if the container fails to start.

## Local development

Copy `.env.local` (already in repo with instructions) and adjust for your setup. Two modes are documented there:
- **Native**: `go run ./cmd/main.go`
- **Docker Compose**: `docker compose up`
