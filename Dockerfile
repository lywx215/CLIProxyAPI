FROM golang:1.26-bookworm AS builder

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends build-essential git && rm -rf /var/lib/apt/lists/*

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
      [ "$_ver" = "dev" ] && [ -n "$BUILD_INFO_VERSION" ] && _ver="$BUILD_INFO_VERSION"; \
      [ "$_commit" = "none" ] && [ -n "$BUILD_INFO_COMMIT" ] && _commit="$BUILD_INFO_COMMIT"; \
      [ "$_date" = "unknown" ] && [ -n "$BUILD_INFO_DATE" ] && _date="$BUILD_INFO_DATE"; \
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
    CGO_ENABLED=1 GOOS=linux go build -buildvcs=false \
      -ldflags="-s -w -X 'main.Version=${_ver}' -X 'main.Commit=${_commit}' -X 'main.BuildDate=${_date}'" \
      -o ./CLIProxyAPI ./cmd/server/

FROM debian:bookworm

RUN apt-get update && apt-get install -y --no-install-recommends tzdata ca-certificates && rm -rf /var/lib/apt/lists/*

RUN mkdir /CLIProxyAPI

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

COPY config.yaml /CLIProxyAPI/config.yaml

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai
ENV OBJECTSTORE_LOCAL_PATH=/data

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["sh", "-c", "[ -f config.yaml ] || cp config.example.yaml config.yaml; ./CLIProxyAPI"]
