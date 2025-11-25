# Build stage
FROM golang:alpine AS builder

# Install SSL certificates (required for HTTPS requests)
RUN apk --no-cache add ca-certificates

WORKDIR /build

# Copy go.mod and go.sum (if it exists)
COPY go.mod ./

# Download dependencies
RUN go mod download

# Copy source code
COPY main.go ./

# Build the binary with static linking
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -installsuffix cgo -ldflags="-w -s" -o adguard-sidecar .

# Final stage - use scratch for minimal image size
FROM scratch

# Copy SSL certificates from builder
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy the binary from builder
COPY --from=builder /build/adguard-sidecar /adguard-sidecar

# Run the binary
ENTRYPOINT ["/adguard-sidecar"]
