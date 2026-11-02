# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS builder

WORKDIR /app

RUN apk add --no-cache ca-certificates git

COPY proxy/go.mod ./
RUN go mod download

COPY proxy/ ./

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /jellyproxy ./cmd/jellyproxy

# Final lightweight production image
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /jellyproxy /app/jellyproxy

ENV PORT=8080 \
    UPSTREAM_URL=http://localhost:8096 \
    SHARED_SECRET="" \
    CACHE_DIR=/var/cache/jellyproxy \
    LOG_LEVEL=info

EXPOSE 8080

ENTRYPOINT ["/app/jellyproxy"]
