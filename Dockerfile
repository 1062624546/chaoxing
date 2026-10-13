# syntax=docker/dockerfile:1
FROM golang:1.27.2-alpine AS builder
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY *.go ./
COPY resource/font_map_table.json ./resource/font_map_table.json
ARG VERSION=go-dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go test ./... && go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/chaoxing .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 10001 chaoxing && adduser -D -u 10001 -G chaoxing chaoxing \
    && mkdir -p /app /config /logs && chown -R 10001:10001 /app /config /logs
WORKDIR /app
ENV CHAOXING_COOKIES_PATH=/config/cookies.txt CHAOXING_LOG_PATH=/logs/chaoxing.log
COPY --from=builder /out/chaoxing /app/chaoxing
COPY --chown=10001:10001 config_template.ini /app/config_template.ini
USER 10001:10001
ENTRYPOINT ["/app/chaoxing"]
CMD ["-c", "/config/config.ini"]
