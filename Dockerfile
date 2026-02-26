# Build stage
FROM golang:1.22.10-alpine3.21 AS builder

WORKDIR /app

# Install git for go mod download
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o telix ./cmd/telix

# Runtime stage
FROM alpine:3.21

# Add non-root user
RUN addgroup -g 1000 telix && \
    adduser -u 1000 -G telix -s /bin/sh -D telix

# Create directories
RUN mkdir -p /var/log/telix /etc/telix && \
    chown -R telix:telix /var/log/telix /etc/telix

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
