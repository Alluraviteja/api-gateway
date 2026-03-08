# -------- Stage 1: Build --------
FROM golang:1.22-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o api-gateway ./cmd/main.go


# -------- Stage 2: Runtime --------
FROM alpine:3.20

WORKDIR /app

# Install wget for health check probe.
RUN apk add --no-cache wget

# Create non-root user
RUN adduser -D appuser

COPY --from=builder /app/api-gateway .

USER appuser

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:8080/livez || exit 1

CMD ["./api-gateway"]
