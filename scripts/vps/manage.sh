#!/usr/bin/env bash
# Manage one isolated Compose project without changing the host's Docker or panel.
set -euo pipefail
umask 077

action=${1:?Missing action}
root=${2:?Missing deployment directory}
project=${3:?Missing Compose project}
host_port=${4:?Missing host port}
upload=${5:?Missing upload directory}
lines=${6:-100}
follow=${7:-0}
domain=${8:-}

fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }
[[ $root =~ ^/[a-zA-Z0-9_./-]+$ && $root != *'/../'* && $root != */.. ]] || fail 'Invalid deployment directory.'
[[ $project =~ ^[a-z][a-z0-9_-]{0,47}$ ]] || fail 'Invalid Compose project.'
[[ $host_port =~ ^[0-9]+$ && $host_port -gt 0 && $host_port -le 65535 ]] || fail 'Invalid host port.'
[[ $lines =~ ^[0-9]+$ && $lines -gt 0 && $lines -le 100000 ]] || fail 'Invalid log line count.'
command -v docker >/dev/null || fail 'Docker must already be installed.'
docker info >/dev/null 2>&1 || fail 'Docker is unavailable; use a Docker-authorized user or --sudo.'
if docker compose version >/dev/null 2>&1; then
    compose=(docker compose)
elif command -v docker-compose >/dev/null && docker-compose version --short | grep -Eq '^v?([2-9]|[1-9][0-9]+)\.'; then
    compose=(docker-compose)
else
    fail 'Docker Compose v2 or later must already be installed.'
fi

if [[ $action == probe ]]; then
    uname -sm
    grep -E '^(PRETTY_NAME|VERSION_ID)=' /etc/os-release || true
    docker --version
    "${compose[@]}" version
    df -h / /tmp
    free -h || true
    docker ps --format 'table {{.Names}}\t{{.Image}}\t{{.Ports}}'
    if command -v ss >/dev/null; then ss -lnt "( sport = :$host_port )"; fi
    if [[ -e $root ]]; then printf 'Deployment directory already exists: %s\n' "$root"; fi
    exit 0
fi

case "$action" in deploy|update|restart|logs|status) ;; *) fail 'Unsupported action.' ;; esac
[[ $root != / && $root != /opt && $root != /home && $root != /root && $root != /tmp ]] || fail 'Use a dedicated deployment directory.'
[[ ! -L $root ]] || fail 'Deployment directory cannot be a symlink.'
if [[ ! -f $root/.cliproxyapi-vps ]]; then
    [[ $action == deploy ]] || fail 'No existing deployment; run deploy first.'
    mkdir -p "$root"
    [[ -z $(find "$root" -mindepth 1 -maxdepth 1 -print -quit) ]] || fail 'Refusing to adopt a nonempty directory.'
    printf '%s\n' "$project" > "$root/.cliproxyapi-vps"
fi
[[ $(cat "$root/.cliproxyapi-vps") == "$project" ]] || fail 'Deployment directory belongs to another Compose project.'
for path in shared releases current.env compose.yaml; do
    [[ ! -L $root/$path ]] || fail "Refusing symlink: $root/$path"
done
current=$root/current.env
compose_file=$root/compose.yaml
compose_run() {
    local env_file=$1
    shift
    env -u CPA_IMAGE -u CPA_PORT "${compose[@]}" --project-directory "$root" -p "$project" \
        --env-file "$env_file" -f "$compose_file" "$@"
}

wait_healthy() {
    local env_file=$1 container state attempt
    for ((attempt = 0; attempt < 60; attempt++)); do
        container=$(compose_run "$env_file" ps -a -q cli-proxy-api) || return 1
        [[ -n $container ]] || return 1
        state=$(docker inspect --format '{{.State.Running}} {{if .State.Health}}{{.State.Health.Status}}{{end}}' "$container") || return 1
        case "$state" in
            'true healthy') return 0 ;;
            'false '*|'true unhealthy') return 1 ;;
        esac
        sleep 2
    done
    return 1
}

if [[ $action == logs || $action == status ]]; then
    [[ -f $current ]] || fail 'No successful deployment yet.'
    if [[ $action == logs ]]; then
        log_args=(logs --tail "$lines")
        [[ $follow != 1 ]] || log_args+=(--follow)
        compose_run "$current" "${log_args[@]}" cli-proxy-api
    else
        [[ ! -f $root/current-manifest.json ]] || cat "$root/current-manifest.json"
        compose_run "$current" ps
        printf 'BaoTa reverse proxy target: http://127.0.0.1:%s\n' "$(sed -n 's/^CPA_PORT=//p' "$current")"
    fi
    exit 0
fi

command -v flock >/dev/null || fail 'flock (util-linux) is required.'
exec 9>"$root/.deploy.lock"
flock -n 9 || fail 'Another deployment or restart is in progress.'
if [[ $action == restart ]]; then
    [[ -f $current ]] || fail 'No successful deployment yet.'
    compose_run "$current" restart cli-proxy-api
    wait_healthy "$current" || fail 'Restarted container did not become healthy; inspect logs.'
    printf 'Restart completed; container is healthy.\n'
    exit 0
fi

[[ $upload =~ ^/tmp/cliproxyapi-upload\.[a-zA-Z0-9]+$ ]] || fail 'Invalid upload directory.'
(cd "$upload" && sha256sum -c source.sha256)
# This file is generated locally and transferred over the authenticated SSH link.
source "$upload/release.env"
[[ $RELEASE =~ ^[0-9]{8}T[0-9]{12}Z-[a-f0-9]{12}$ && $COMMIT =~ ^[a-f0-9]{40,64}$ ]] || fail 'Invalid release metadata.'
release=$root/releases/$RELEASE
[[ ! -e $release ]] || fail 'Release already exists.'
mkdir -p "$release/source"
tar --no-same-owner --no-same-permissions -xzf "$upload/source.tar.gz" -C "$release/source"
cp "$release/source/deploy-manifest.json" "$release/manifest.json"
image=$project:$RELEASE
printf 'Building local source snapshot: %s\n' "$RELEASE"
docker build --build-arg "VERSION=$VERSION" --build-arg "COMMIT=$COMMIT" --build-arg "BUILD_DATE=$BUILD_DATE" \
    --label "org.opencontainers.image.revision=$COMMIT" \
    --label "io.cliproxyapi.source-snapshot=$SNAPSHOT" \
    -t "$image" "$release/source"

mkdir -p "$root/shared"/{auths,logs,plugins,data}
if [[ ! -f $root/shared/config.yaml ]]; then
    management_key=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
    client_key=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
    cat > "$root/shared/config.yaml" <<CONFIG
config-version: 8
server:
  host: "0.0.0.0"
  port: 8317
management:
  allow-remote: true
  secret-key: "$management_key"
  disable-control-panel: false
  disable-auto-update-panel: true
access:
  api-keys:
    - "$client_key"
oauth:
  auth-dir: "/root/.cli-proxy-api"
observability:
  logs:
    logging-to-file: true
    logs-max-total-size-mb: 256
CONFIG
    printf 'Management key: %s\nClient API key: %s\n' "$management_key" "$client_key" > "$root/shared/initial-credentials.txt"
    unset management_key client_key
    chmod 600 "$root/shared/config.yaml" "$root/shared/initial-credentials.txt"
    printf 'Initial keys saved privately to %s/shared/initial-credentials.txt (mode 600).\n' "$root"
fi
if [[ ! -f $compose_file ]]; then
    cat > "$compose_file" <<'COMPOSE'
services:
  cli-proxy-api:
    image: ${CPA_IMAGE:?Missing deployment image}
    pull_policy: never
    restart: unless-stopped
    ports:
      - "127.0.0.1:${CPA_PORT:-8317}:8317"
    volumes:
      - ./shared/config.yaml:/CLIProxyAPI/config.yaml
      - ./shared/auths:/root/.cli-proxy-api
      - ./shared/logs:/CLIProxyAPI/logs
      - ./shared/plugins:/CLIProxyAPI/plugins
      - ./shared/data:/data
    environment:
      TZ: Asia/Shanghai
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8317/healthz"]
      interval: 5s
      timeout: 3s
      retries: 12
      start_period: 10s
COMPOSE
fi
if [[ -f $current ]]; then
    cp "$current" "$release/previous.env"
    # Keep the configured host port on updates, including non-default ports.
    host_port=$(sed -n 's/^CPA_PORT=//p' "$current")
    [[ $host_port =~ ^[0-9]+$ ]] || fail 'Missing saved host port.'
fi
candidate=$release/candidate.env
printf 'CPA_IMAGE=%s\nCPA_PORT=%s\n' "$image" "$host_port" > "$candidate"
compose_run "$candidate" config -q
if compose_run "$candidate" up -d --no-build --pull never cli-proxy-api && wait_healthy "$candidate"; then
    mv "$candidate" "$current"
    cp "$release/manifest.json" "$root/current-manifest.json"
    printf 'Deployment completed; container is healthy.\nBaoTa reverse proxy target: http://127.0.0.1:%s\n' "$host_port"
    if [[ -n $domain ]]; then
        bash "$upload/proxy.sh" "$root" "$project" "$domain" "$host_port"
    fi
else
    if [[ -f $current ]]; then
        printf 'Deployment failed; restoring the previous image.\n' >&2
        if compose_run "$current" up -d --no-build --pull never cli-proxy-api && wait_healthy "$current"; then
            printf 'Previous image restored and healthy.\n' >&2
        else
            printf 'Rollback also failed; inspect Docker and application logs.\n' >&2
        fi
    else
        compose_run "$candidate" stop cli-proxy-api || true
    fi
    fail 'Candidate deployment did not become healthy.'
fi
