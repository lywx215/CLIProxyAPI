#!/usr/bin/env bash
# Exercise production deployment control flow with a fake Docker executable.
set -euo pipefail
manager=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
scenario=${2:?Missing test scenario}
fixture=$(mktemp -d /tmp/cpa-manager-test.XXXXXXXX)
upload=$(mktemp -d /tmp/cliproxyapi-upload.XXXXXXXX)
trap 'rm -rf -- "$fixture" "$upload"' EXIT
export FIXTURE=$fixture SCENARIO=$scenario
mkdir -p "$fixture/bin" "$fixture/app/shared/auths" "$fixture/source"
root=$fixture/app
printf 'test-vps\n' > "$root/.cliproxyapi-vps"
printf 'CPA_IMAGE=previous:good\nCPA_PORT=9321\n' > "$root/current.env"
cp "$root/current.env" "$fixture/previous.env"
printf 'existing credentials\n' > "$root/shared/config.yaml"
printf 'account data\n' > "$root/shared/auths/account.json"
printf 'services: {}\n' > "$root/compose.yaml"
printf '{"version":"previous"}\n' > "$root/current-manifest.json"
printf '{"version":"candidate"}\n' > "$fixture/source/deploy-manifest.json"
tar -czf "$upload/source.tar.gz" -C "$fixture/source" .
(cd "$upload" && sha256sum source.tar.gz > source.sha256)
cat > "$upload/release.env" <<'ENV'
RELEASE=20261028T120000000000Z-123456789abc
COMMIT=1111111111111111111111111111111111111111
VERSION=local-test
BUILD_DATE=2026-10-28T12:00:00Z
SNAPSHOT=123456789abcdef
ENV
cat > "$fixture/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FIXTURE/events"
case "$1" in
    info) exit 0 ;;
    build) [[ $SCENARIO != build-failure ]]; exit ;;
    inspect)
        if [[ $SCENARIO == health-failure && $(cat "$FIXTURE/active") != previous:good ]]; then
            printf 'true unhealthy\n'
        else
            printf 'true healthy\n'
        fi
        exit 0 ;;
    compose)
        shift
        if [[ $1 == version ]]; then exit 0; fi
        env_file=''
        while (($#)); do
            case "$1" in
                --env-file) env_file=$2; shift 2 ;;
                --project-directory|-p|-f) shift 2 ;;
                *) break ;;
            esac
        done
        case "$1" in
            config) exit 0 ;;
            up) sed -n 's/^CPA_IMAGE=//p' "$env_file" > "$FIXTURE/active"; exit 0 ;;
            ps) printf 'fake-container\n'; exit 0 ;;
            *) exit 0 ;;
        esac ;;
esac
exit 2
DOCKER
chmod +x "$fixture/bin/docker"
printf '#!/usr/bin/env bash\nexit 0\n' > "$fixture/bin/flock"
chmod +x "$fixture/bin/flock"
export PATH=$fixture/bin:$PATH
result=0
bash "$manager" update "$root" test-vps 8317 "$upload" > "$fixture/output" 2>&1 || result=$?
cat "$fixture/output"
[[ $(cat "$root/shared/config.yaml") == 'existing credentials' ]]
[[ $(cat "$root/shared/auths/account.json") == 'account data' ]]
case "$scenario" in
    build-failure)
        [[ $result != 0 && ! -e $fixture/active ]]
        grep -q '^build ' "$fixture/events"
        cmp "$root/current.env" "$fixture/previous.env" ;;
    health-failure)
        [[ $result != 0 && $(cat "$fixture/active") == previous:good ]]
        cmp "$root/current.env" "$fixture/previous.env"
        grep -q 'previous' "$root/current-manifest.json" ;;
    success)
        [[ $result == 0 ]]
        grep -q '^CPA_PORT=9321$' "$root/current.env"
        grep -q '^CPA_IMAGE=test-vps:20261028T120000000000Z-123456789abc$' "$root/current.env"
        grep -q 'candidate' "$root/current-manifest.json" ;;
    *) exit 2 ;;
esac
