FROM golang:1.25-alpine AS builder
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /agent-bridge ./cmd/agent-bridge

FROM alpine:3.21
LABEL org.opencontainers.image.source="https://github.com/markus-barta/fleetcom"
# docker CLI is needed so the tailer can `docker logs -f` the sibling
# container. If your host mounts /var/run/docker.sock into this
# container, `docker` talks to the host daemon directly.
RUN apk add --no-cache ca-certificates tzdata docker-cli
COPY --from=builder /agent-bridge /usr/local/bin/agent-bridge
EXPOSE 9180
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://localhost:9180/healthz || exit 1
ENTRYPOINT ["agent-bridge"]
