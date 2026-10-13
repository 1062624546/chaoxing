# Use the bundled BuildKit frontend to avoid an extra Docker Hub frontend pull.
ARG GO_BUILD_IMAGE=m.daocloud.io/docker.io/library/golang:1.27.2-alpine
ARG GO_RUNTIME_IMAGE=m.daocloud.io/docker.io/library/alpine:3.23
FROM ${GO_BUILD_IMAGE} AS builder
WORKDIR /src
ARG GO_MODULE_PROXY=https://goproxy.cn
ENV CGO_ENABLED=0 GOPROXY=${GO_MODULE_PROXY} GOSUMDB=sum.golang.org GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY *.go ./
COPY resource/font_map_table.json ./resource/font_map_table.json
ARG VERSION=go-dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go test ./... && go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/chaoxing .

FROM ${GO_RUNTIME_IMAGE}
ARG ALPINE_MIRROR=https://mirrors.aliyun.com/alpine
RUN sed -i "s|https\?://dl-cdn.alpinelinux.org/alpine|${ALPINE_MIRROR}|g" /etc/apk/repositories \
    && apk add --no-cache ca-certificates tzdata \
    && addgroup -g 10001 chaoxing && adduser -D -u 10001 -G chaoxing chaoxing \
    && mkdir -p /app /config /logs && chown -R 10001:10001 /app /config /logs
WORKDIR /app
ENV CHAOXING_COOKIES_PATH=/config/cookies.txt CHAOXING_LOG_PATH=/logs/chaoxing.log
COPY --from=builder /out/chaoxing /app/chaoxing
COPY --chown=10001:10001 config_template.ini /app/config_template.ini
USER 10001:10001
ENTRYPOINT ["/app/chaoxing"]
CMD ["-c", "/config/config.ini"]
