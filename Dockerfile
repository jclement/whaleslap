# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git ca-certificates

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build arguments for version info
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
    -o /whaleslap ./cmd/whaleslap

# Runtime stage
FROM alpine:3.21

RUN apk add --no-cache ca-certificates docker-cli docker-cli-compose

COPY --from=builder /whaleslap /usr/local/bin/whaleslap

# Create non-root user (but we need docker socket access)
RUN addgroup -g 1000 whaleslap && \
    adduser -u 1000 -G whaleslap -D whaleslap

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/whaleslap"]
