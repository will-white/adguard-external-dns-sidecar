# Build stage
# --platform=$BUILDPLATFORM keeps the compiler running natively and cross-compiles
# via GOOS/GOARCH, which is far faster than emulating the target under QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine3.24 AS builder

# Install SSL certificates (required for HTTPS requests)
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /build

# Copy go.mod (no dependencies to download - stdlib only)
COPY go.mod ./

# Copy source code (*_test.go is excluded by .dockerignore)
COPY *.go ./

# Build the binary with static linking for the target platform.
# TARGETOS/TARGETARCH are supplied automatically by BuildKit. Declare them with
# NO default: a default value wins over the automatic one, which silently
# produces an amd64 binary inside an arm64 image.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-w -s" -o adguard-sidecar .

# Final stage - use scratch for minimal image size
FROM scratch

# Add labels for OCI compliance
LABEL org.opencontainers.image.title="AdGuard External-DNS Sidecar"
LABEL org.opencontainers.image.description="Sidecar that maintains a managed AdGuard Home rewrite rule from Kubernetes Ingress hosts"
LABEL org.opencontainers.image.source="https://github.com/will-white/adguard-external-dns-sidecar"

# Copy timezone data for proper time handling
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# Copy SSL certificates from builder
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy the binary from builder
COPY --from=builder /build/adguard-sidecar /adguard-sidecar

# Expose health check port (matches the HEALTH_PORT default)
EXPOSE 8080

# Run as non-root user (65534 is the "nobody" user)
USER 65534:65534

# Health check - the binary probes itself, since scratch has no shell or curl.
# Exec form: the process exit code is the health result (no `|| exit 1` needed,
# and shell operators would not be interpreted here anyway).
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/adguard-sidecar", "-health"]

# Run the binary
ENTRYPOINT ["/adguard-sidecar"]
