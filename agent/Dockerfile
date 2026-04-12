FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /fleetcom-agent ./cmd/fleetcom-agent

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /fleetcom-agent /usr/local/bin/fleetcom-agent
ENTRYPOINT ["fleetcom-agent"]
