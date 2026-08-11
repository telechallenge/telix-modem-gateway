# Build stage
FROM golang:1.24-alpine3.21 AS builder

WORKDIR /app

# Install git for go mod download
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Stamp the build. Without this the container reports version "1.0.0" — the
# source default — in the banner, in ATI and in the Grafana Version panel, so
# there is no way to tell a freshly deployed gateway from a stale one. That is
# not cosmetic: diagnosing a BBS-specific transfer fault means changing gateway
# behaviour and re-testing, and a silently stale container makes every result a
# lie. Defaults to "dev" so a plain `docker build` still works.
ARG VERSION=dev

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo \
    -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o telix ./cmd/telix

# Runtime stage
FROM alpine:3.21

# Add non-root user
RUN addgroup -g 1000 telix && \
    adduser -u 1000 -G telix -s /bin/sh -D telix

# Create directories
RUN mkdir -p /app/logs /etc/telix && \
    chown -R telix:telix /app/logs /etc/telix

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/telix .

# Copy default config
COPY configs/telix.yaml /etc/telix/telix.yaml

# Set ownership
RUN chown -R telix:telix /app

# Switch to non-root user
USER telix

# Expose default port
EXPOSE 2323

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD nc -z localhost 2323 || exit 1

# Run
ENTRYPOINT ["./telix"]
CMD ["-config", "/etc/telix/telix.yaml"]
