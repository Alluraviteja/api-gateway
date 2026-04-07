# syntax=docker/dockerfile:1

# -------- Stage 1: Build --------
FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o api-gateway ./cmd/main.go


# -------- Stage 2: Runtime --------
FROM alpine:3.21

WORKDIR /app

# Add metadata for traceability
ARG VERSION=dev
ARG GIT_COMMIT=unknown
LABEL org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${GIT_COMMIT}" \
      org.opencontainers.image.title="api-gateway" \
      org.opencontainers.image.base.name="alpine:3.21"

# Install wget for health check probe
RUN apk add --no-cache wget

# Create non-root user
RUN adduser -D appuser

COPY --from=builder /app/api-gateway .

USER appuser

EXPOSE 8080

CMD ["./api-gateway"]
