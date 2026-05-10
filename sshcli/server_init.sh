#!/bin/bash
# ============================================================
# CLIProxyAPI — 新服务器初始化脚本
# 由 vps_deploy.py --init 自动上传并执行
# 兼容 Ubuntu/Debian 和 CentOS/RHEL
# ============================================================
set -euo pipefail

DEPLOY_PATH="${1:-/opt/cliproxy}"

echo "========================================"
echo "  CLIProxyAPI 服务器初始化"
echo "  部署目录: ${DEPLOY_PATH}"
echo "========================================"
echo ""

# ==================== 1. 系统包管理器检测 ====================
PKG_MGR=""
if command -v apt-get &>/dev/null; then
    PKG_MGR="apt"
elif command -v yum &>/dev/null; then
    PKG_MGR="yum"
elif command -v dnf &>/dev/null; then
    PKG_MGR="dnf"
else
    echo "⚠️  未识别的包管理器，跳过系统更新"
fi

# ==================== 2. 安装基础工具 ====================
echo "[1/5] 安装基础工具..."
if [ "$PKG_MGR" = "apt" ]; then
    apt-get update -qq
    apt-get install -y -qq curl wget ca-certificates gnupg lsb-release
elif [ "$PKG_MGR" = "yum" ] || [ "$PKG_MGR" = "dnf" ]; then
    $PKG_MGR install -y -q curl wget ca-certificates
fi
echo "  ✅ 基础工具就绪"

# ==================== 3. 安装 Docker ====================
echo "[2/5] 检查/安装 Docker..."
if ! command -v docker &>/dev/null; then
    echo "  正在安装 Docker..."
    curl -fsSL https://get.docker.com | bash
    systemctl enable docker
    systemctl start docker
    echo "  ✅ Docker 安装完成: $(docker --version)"
else
    echo "  ℹ️  Docker 已安装: $(docker --version)"
    # 确保 Docker 正在运行
    if ! systemctl is-active --quiet docker; then
        systemctl start docker
        echo "  ✅ Docker 服务已启动"
    fi
fi

# 安装 Docker Compose plugin（如果不存在）
echo "[3/5] 检查 Docker Compose..."
if ! docker compose version &>/dev/null; then
    echo "  正在安装 Docker Compose plugin..."
    if [ "$PKG_MGR" = "apt" ]; then
        apt-get install -y -qq docker-compose-plugin 2>/dev/null || true
    elif [ "$PKG_MGR" = "yum" ] || [ "$PKG_MGR" = "dnf" ]; then
        $PKG_MGR install -y -q docker-compose-plugin 2>/dev/null || true
    fi
    # 如果 plugin 方式失败，尝试独立安装
    if ! docker compose version &>/dev/null; then
        COMPOSE_VERSION=$(curl -s https://api.github.com/repos/docker/compose/releases/latest | grep tag_name | cut -d '"' -f 4)
        curl -fsSL "https://github.com/docker/compose/releases/download/${COMPOSE_VERSION}/docker-compose-$(uname -s)-$(uname -m)" \
            -o /usr/local/bin/docker-compose
        chmod +x /usr/local/bin/docker-compose
        ln -sf /usr/local/bin/docker-compose /usr/libexec/docker/cli-plugins/docker-compose 2>/dev/null || true
    fi
    echo "  ✅ Docker Compose 安装完成"
else
    echo "  ℹ️  Docker Compose 已安装: $(docker compose version)"
fi

# ==================== 4. 创建目录结构 ====================
echo "[4/5] 创建目录结构..."
mkdir -p "${DEPLOY_PATH}"/{auths,logs}
mkdir -p /data/cliproxy
echo "  ✅ 目录结构已创建:"
echo "      ${DEPLOY_PATH}/"
echo "      ${DEPLOY_PATH}/auths/"
echo "      ${DEPLOY_PATH}/logs/"
echo "      /data/cliproxy/"

# ==================== 5. 系统优化 ====================
echo "[5/5] 系统优化..."

# 增加文件描述符限制（幂等写入）
if ! grep -q "# cliproxy-limits" /etc/security/limits.conf 2>/dev/null; then
    cat >> /etc/security/limits.conf <<'EOF'
# cliproxy-limits
* soft nofile 65535
* hard nofile 65535
EOF
    echo "  ✅ 文件描述符限制已设置"
else
    echo "  ℹ️  文件描述符限制已存在"
fi

# 优化内核参数（幂等写入）
if ! grep -q "# cliproxy-sysctl" /etc/sysctl.conf 2>/dev/null; then
    cat >> /etc/sysctl.conf <<'EOF'
# cliproxy-sysctl
net.core.somaxconn = 32768
net.ipv4.tcp_max_syn_backlog = 16384
net.ipv4.ip_local_port_range = 1024 65535
net.ipv4.tcp_tw_reuse = 1
EOF
    sysctl -p 2>/dev/null || true
    echo "  ✅ 内核参数已优化"
else
    echo "  ℹ️  内核参数已优化"
fi

# 防火墙配置
echo "  配置防火墙..."
if command -v ufw &>/dev/null; then
    ufw allow 8317/tcp 2>/dev/null || true
    ufw allow 8085/tcp 2>/dev/null || true
    echo "  ✅ UFW 规则已添加"
elif command -v firewall-cmd &>/dev/null; then
    firewall-cmd --permanent --add-port=8317/tcp 2>/dev/null || true
    firewall-cmd --permanent --add-port=8085/tcp 2>/dev/null || true
    firewall-cmd --reload 2>/dev/null || true
    echo "  ✅ firewalld 规则已添加"
else
    echo "  ℹ️  未检测到防火墙，跳过"
fi

echo ""
echo "========================================"
echo "  ✅ 初始化完成！"
echo ""
echo "  部署目录:    ${DEPLOY_PATH}"
echo "  Docker:      $(docker --version 2>/dev/null || echo 'N/A')"
echo "  Compose:     $(docker compose version 2>/dev/null || echo 'N/A')"
echo "========================================"
