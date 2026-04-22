#!/usr/bin/env bash
#
# update.sh - CLIProxyAPI 一键更新脚本
#
# 用法: bash /www/server/panel/data/compose/cli-proxy-api/update.sh
#

set -euo pipefail

# ===================== 配置 =====================
REPO_DIR="/opt/CLIProxyAPI"
REPO_URL="https://github.com/lywx215/CLIProxyAPI.git"
COMPOSE_DIR="/www/server/panel/data/compose/cli-proxy-api"
IMAGE_NAME="cli-proxy-api:local"
# =================================================

echo "========================================"
echo "  CLIProxyAPI 一键更新"
echo "  $(date '+%Y-%m-%d %H:%M:%S')"
echo "========================================"

# 1. 拉取最新代码
echo ""
echo "[1/4] 📥 拉取最新代码..."
if [ -d "${REPO_DIR}/.git" ]; then
    cd "${REPO_DIR}"
    git fetch origin
    git reset --hard origin/main
    echo "  ✅ 代码已更新到最新"
else
    echo "  📂 首次克隆仓库..."
    rm -rf "${REPO_DIR}"
    git clone "${REPO_URL}" "${REPO_DIR}"
    echo "  ✅ 仓库克隆完成"
fi

cd "${REPO_DIR}"
VERSION=$(git describe --tags --always 2>/dev/null || echo "dev")
COMMIT=$(git rev-parse --short HEAD)
BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "  版本: ${VERSION} (${COMMIT})"

# 2. 构建 Docker 镜像
echo ""
echo "[2/4] 🔨 构建 Docker 镜像..."
docker build \
    --build-arg VERSION="${VERSION}" \
    --build-arg COMMIT="${COMMIT}" \
    --build-arg BUILD_DATE="${BUILD_DATE}" \
    -t "${IMAGE_NAME}" . 2>&1 | tail -5
echo "  ✅ 镜像构建完成: ${IMAGE_NAME}"

# 3. 重启服务
echo ""
echo "[3/4] 🔄 重启服务..."
cd "${COMPOSE_DIR}"
docker compose up -d --force-recreate
echo "  ✅ 服务已重启"

# 4. 清理旧镜像
echo ""
echo "[4/4] 🧹 清理悬空镜像..."
docker image prune -f 2>/dev/null || true

# 完成
echo ""
echo "========================================"
echo "  ✅ 更新完成！"
echo "  镜像: ${IMAGE_NAME}"
echo "  版本: ${VERSION} (${COMMIT})"
echo "========================================"
echo ""
echo "查看日志: docker compose -f ${COMPOSE_DIR}/docker-compose.yml logs -f"
