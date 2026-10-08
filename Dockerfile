# Build:
#   docker build --build-arg VERSION=<v> -t outpostimage:<tag> .
# Optional: --build-arg GOPROXY=https://goproxy.cn,direct when proxy.golang.org
# is unreachable (go.sum still verifies every module).
#
# Base images come from the risecloud namespace on Huawei SWR (public,
# amd64+arm64), never directly from Docker Hub. Guarded by
# deploy/docker/dockerfile_test.go.
ARG SWR=swr.cn-east-3.myhuaweicloud.com/risecloud

# Build stage
FROM ${SWR}/golang:1.25-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git make

WORKDIR /build

# Copy go mod files first for better caching
ARG GOPROXY=https://proxy.golang.org,direct
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary — VERSION is injected at build time via --build-arg
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -ldflags="-s -w -X main.version=${VERSION}" -o outpostimage .

# Runtime stage
FROM ${SWR}/alpine:3.19

# Install ca-certificates for HTTPS registry interception
RUN apk add --no-cache ca-certificates tzdata

# Create non-root user
RUN addgroup -g 1000 outpostimage && \
    adduser -D -u 1000 -G outpostimage outpostimage

# Create directories
RUN mkdir -p /etc/outpostimage /var/lib/docker && \
    chown -R outpostimage:outpostimage /etc/outpostimage

# Copy binary from builder
COPY --from=builder /build/outpostimage /usr/local/bin/

# Set up CA directory for generated certificates
RUN mkdir -p /etc/outpostimage && \
    chown outpostimage:outpostimage /etc/outpostimage

# Switch to non-root user
USER outpostimage

# Expose ports
# :5000 - P2P Registry API (OCI Distribution Spec)
# :3128 - HTTP Proxy
# :9090 - Prometheus Metrics
EXPOSE 5000 3128 9090

# Health checks
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:9090/healthz || exit 1

# Default entrypoint
ENTRYPOINT ["/usr/local/bin/outpostimage"]
CMD ["registry"]
