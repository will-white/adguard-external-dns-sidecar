# Build stage
FROM golang:1.23-alpine3.20 AS builder

# Install SSL certificates (required for HTTPS requests)
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /build

# Copy go.mod and go.sum (if it exists)
COPY go.mod ./

# Download dependencies
RUN go mod download

# Copy source code
COPY main.go ./

# Build the binary with static linking for multiple architectures
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -a -installsuffix cgo -ldflags="-w -s" -o adguard-sidecar .

# Final stage - use scratch for minimal image size
FROM scratch

# Add labels for OCI compliance
LABEL org.opencontainers.image.title="AdGuard External-DNS Sidecar"
LABEL org.opencontainers.image.description="Sidecar to enforce DNS rule positioning in AdGuard Home"
LABEL org.opencontainers.image.source="https://github.com/will-white/adguard-external-dns-sidecar"

# Copy timezone data for proper time handling
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# Copy SSL certificates from builder
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy the binary from builder
COPY --from=builder /build/adguard-sidecar /adguard-sidecar

# Expose health check port
EXPOSE 8080

# Run as non-root user (65534 is the "nobody" user)
USER 65534:65534

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/adguard-sidecar", "-health"] || exit 1

# Run the binary
ENTRYPOINT ["/adguard-sidecar"]
