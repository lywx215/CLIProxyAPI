FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

# Auto-detect version info: build-arg > .build-info > git > defaults
RUN set -e; \
    _ver="$VERSION"; _commit="$COMMIT"; _date="$BUILD_DATE"; \
    if [ -f .build-info ]; then \
      . ./.build-info 2>/dev/null || true; \
      if [ "$_ver" = "dev" ] && [ -n "$BUILD_INFO_VERSION" ]; then _ver="$BUILD_INFO_VERSION"; fi; \
      if [ "$_commit" = "none" ] && [ -n "$BUILD_INFO_COMMIT" ]; then _commit="$BUILD_INFO_COMMIT"; fi; \
      if [ "$_date" = "unknown" ] && [ -n "$BUILD_INFO_DATE" ]; then _date="$BUILD_INFO_DATE"; fi; \
    fi; \
    if [ "$_ver" = "dev" ] && command -v git >/dev/null 2>&1 && [ -d .git ]; then \
      _ver=$(git describe --tags --always 2>/dev/null || echo "dev"); \
    fi; \
    if [ "$_commit" = "none" ] && command -v git >/dev/null 2>&1 && [ -d .git ]; then \
      _commit=$(git rev-parse --short HEAD 2>/dev/null || echo "none"); \
    fi; \
    if [ "$_date" = "unknown" ]; then \
      _date=$(date -u +"%Y-%m-%dT%H:%M:%SZ"); \
    fi; \
    echo "Building with VERSION=${_ver} COMMIT=${_commit} BUILD_DATE=${_date}"; \
    CGO_ENABLED=0 GOOS=linux go build \
      -buildvcs=false \
      -ldflags="-s -w -X 'main.Version=${_ver}' -X 'main.Commit=${_commit}' -X 'main.BuildDate=${_date}'" \
      -o ./CLIProxyAPI ./cmd/server/

FROM alpine:3.23

RUN apk add --no-cache tzdata ca-certificates

RUN mkdir /CLIProxyAPI

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

# Use wildcard so it copies config.example.yaml, and config.yaml *if it exists*, avoiding build failures when config.yaml is excluded from git
COPY config*.yaml /CLIProxyAPI/

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai
ENV OBJECTSTORE_LOCAL_PATH=/data

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["sh", "-c", "[ -f config.yaml ] || cp config.example.yaml config.yaml; ./CLIProxyAPI"]
