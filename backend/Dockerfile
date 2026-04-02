FROM golang:1.25-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /fleetcom-server ./cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /fleetcom-server /usr/local/bin/fleetcom-server
COPY static/ /app/static/
WORKDIR /app

EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://localhost:8090/healthz || exit 1
ENTRYPOINT ["fleetcom-server"]
