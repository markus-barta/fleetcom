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
RUN apk add --no-cache ca-certificates tzdata fastfetch
COPY --from=builder /fleetcom-bosun /usr/local/bin/fleetcom-bosun
ENTRYPOINT ["fleetcom-bosun"]
