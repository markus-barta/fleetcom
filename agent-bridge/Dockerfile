FROM golang:1.25-alpine AS builder
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /agent-bridge ./cmd/agent-bridge

FROM alpine:3.21
LABEL org.opencontainers.image.source="https://github.com/markus-barta/fleetcom"
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /agent-bridge /usr/local/bin/agent-bridge
EXPOSE 9180
# Bridge persists its Ed25519 identity here across restarts.
VOLUME ["/var/lib/fleetcom-agent-bridge"]
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://localhost:9180/healthz || exit 1
ENTRYPOINT ["agent-bridge"]
