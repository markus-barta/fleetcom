FROM golang:1.25-alpine AS builder
ARG VERSION=0.1.0
ARG BUILD_TIME=unknown
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w \
      -X main.Version=${VERSION} \
      -X main.BuildTime=${BUILD_TIME}" \
    -o /fleetcom-bosun ./cmd/fleetcom-bosun

FROM alpine:3.21
# docker-cli: required by FLEET-61/62/63 command handlers (openclaw.pair,
# container.restart, bridge.install) which shell out to `docker exec`
# / `docker restart` / `docker run`. The CLI talks to the host daemon
# via the bind-mounted /var/run/docker.sock — no daemon runs inside.
RUN apk add --no-cache ca-certificates tzdata fastfetch docker-cli
COPY --from=builder /fleetcom-bosun /usr/local/bin/fleetcom-bosun
ENTRYPOINT ["fleetcom-bosun"]
