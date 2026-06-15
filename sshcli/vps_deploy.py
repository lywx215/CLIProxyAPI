#!/usr/bin/env python3
"""
CLIProxyAPI — VPS 自动化部署工具
通过 SSH 将项目部署到多台 VPS 服务器

用法:
    python vps_deploy.py                         # 部署到所有服务器
    python vps_deploy.py --servers vps-01,vps-02 # 部署到指定服务器
    python vps_deploy.py --config-only           # 仅更新配置+重启
    python vps_deploy.py --init vps-01           # 首次初始化服务器
    python vps_deploy.py --status                # 查看所有服务器状态
    python vps_deploy.py --health                # 仅执行健康检查
    python vps_deploy.py --parallel 5            # 设置并行数
"""

import os
import sys
import copy
import yaml
import json
import time
import zipfile
import tempfile
import argparse
from pathlib import Path
from datetime import datetime
from concurrent.futures import ThreadPoolExecutor, as_completed
from fnmatch import fnmatch
from io import StringIO

# Fix Windows GBK console encoding
if sys.stdout and hasattr(sys.stdout, 'reconfigure'):
    try:
        sys.stdout.reconfigure(encoding='utf-8', errors='replace')
    except Exception:
        pass
if sys.stderr and hasattr(sys.stderr, 'reconfigure'):
    try:
        sys.stderr.reconfigure(encoding='utf-8', errors='replace')
    except Exception:
        pass

try:
    import paramiko
except ImportError:
    print("❌ 请先安装 paramiko: pip install paramiko")
    sys.exit(1)

try:
    from scp import SCPClient
except ImportError:
    SCPClient = None  # fallback to SFTP

# ==================== 路径常量 ====================

SCRIPT_DIR = Path(__file__).parent
PROJECT_ROOT = SCRIPT_DIR.parent
CONFIG_FILE = SCRIPT_DIR / "vps_deploy_config.yaml"
INIT_SCRIPT = SCRIPT_DIR / "server_init.sh"
BASE_CONFIG = PROJECT_ROOT / "config.yaml"
BASE_COMPOSE = PROJECT_ROOT / "docker-compose.yml"

# 打包排除规则
EXCLUDE_PATTERNS = [
    ".git", ".git/**", "auths/**", ".env", "config.example.yaml",
    "*.log", "__pycache__", "zeaburcli/**", "sshcli/**",
    "test/**", "UnitTest/**", "diff.txt",
]


# ==================== 日志 ====================

def log(msg: str, level: str = "INFO", prefix: str = ""):
    timestamp = datetime.now().strftime("%H:%M:%S")
    symbols = {"INFO": "ℹ️", "OK": "✅", "WARN": "⚠️", "ERROR": "❌",
               "UPLOAD": "📤", "SSH": "🔗", "DOCKER": "🐳", "CHECK": "🔍"}
    symbol = symbols.get(level, "•")
    tag = f"[{prefix}] " if prefix else ""
    print(f"[{timestamp}] {symbol} {tag}{msg}")


# ==================== 配置加载 ====================

def load_config(config_path: Path = None) -> dict:
    path = config_path or CONFIG_FILE
    if not path.exists():
        log(f"配置文件不存在: {path}", "ERROR")
        sys.exit(1)
    with open(path, "r", encoding="utf-8") as f:
        return yaml.safe_load(f)


def load_base_config() -> dict:
    if not BASE_CONFIG.exists():
        log(f"基础配置不存在: {BASE_CONFIG}", "ERROR")
        sys.exit(1)
    with open(BASE_CONFIG, "r", encoding="utf-8") as f:
        return yaml.safe_load(f)


def deep_merge(base: dict, override: dict) -> dict:
    """深度合并两个字典，override 覆盖 base"""
    result = copy.deepcopy(base)
    for key, value in override.items():
        if key in result and isinstance(result[key], dict) and isinstance(value, dict):
            result[key] = deep_merge(result[key], value)
        else:
            result[key] = copy.deepcopy(value)
    return result


def get_local_version_info() -> dict:
    """从本地 git 获取版本信息"""
    import subprocess
    from datetime import datetime
    info = {"version": "dev", "commit": "none", "build_date": datetime.utcnow().strftime("%Y-%m-%dT%H:%M:%SZ")}
    try:
        info["version"] = subprocess.check_output(
            ["git", "describe", "--tags", "--always"], cwd=str(PROJECT_ROOT),
            stderr=subprocess.DEVNULL, text=True
        ).strip() or "dev"
    except Exception:
        pass
    try:
        info["commit"] = subprocess.check_output(
            ["git", "rev-parse", "--short", "HEAD"], cwd=str(PROJECT_ROOT),
            stderr=subprocess.DEVNULL, text=True
        ).strip() or "none"
    except Exception:
        pass
    return info

# ==================== VPS 部署器 ====================

class VPSDeployer:
    """单台 VPS 的部署器"""

    def __init__(self, server_config: dict, global_config: dict, base_config: dict, version_info: dict = None):
        self.server = server_config
        self.name = server_config["name"]
        self.host = server_config["host"]
        self.port = server_config.get("port", 22)
        self.user = server_config.get("user", "root")
        self.auth = server_config.get("auth", "password")
        self.deploy_path = server_config.get("deploy_path", global_config.get("deploy_path", "/opt/cliproxy"))
        self.docker_image = global_config.get("docker_image", "eceasy/cli-proxy-api:latest")
        self.build_from_source = global_config.get("build_from_source", False)
        self.hc_config = global_config.get("health_check", {})
        self.common_env = global_config.get("common_env", {})
        self.env_overrides = server_config.get("env_overrides", {})
        self.config_overrides = server_config.get("config_overrides", {})
        self.exposed_ports = server_config.get("exposed_ports", None)
        self.base_config = base_config
        self.version_info = version_info or {"version": "dev", "commit": "none", "build_date": "unknown"}
        self.ssh: paramiko.SSHClient = None
        self.sftp: paramiko.SFTPClient = None

    # ---- SSH 连接 ----

    def connect(self):
        log(f"连接 {self.user}@{self.host}:{self.port}", "SSH", self.name)
        self.ssh = paramiko.SSHClient()
        self.ssh.set_missing_host_key_policy(paramiko.AutoAddPolicy())

        connect_kwargs = {
            "hostname": self.host,
            "port": self.port,
            "username": self.user,
            "timeout": 15,
        }
        if self.auth == "key":
            key_file = os.path.expanduser(self.server.get("key_file", "~/.ssh/id_rsa"))
            connect_kwargs["key_filename"] = key_file
        else:
            connect_kwargs["password"] = self.server.get("password", "")

        self.ssh.connect(**connect_kwargs)
        self.sftp = self.ssh.open_sftp()
        log(f"SSH 连接成功", "OK", self.name)

    def disconnect(self):
        if self.sftp:
            self.sftp.close()
        if self.ssh:
            self.ssh.close()
        self.ssh = None
        self.sftp = None

    def exec_cmd(self, cmd: str, timeout: int = 120) -> tuple:
        """执行远程命令，返回 (exit_code, stdout, stderr)"""
        stdin, stdout, stderr = self.ssh.exec_command(cmd, timeout=timeout)
        exit_code = stdout.channel.recv_exit_status()
        out = stdout.read().decode("utf-8", errors="replace").strip()
        err = stderr.read().decode("utf-8", errors="replace").strip()
        return exit_code, out, err

    # ---- 文件操作 ----

    def _sftp_makedirs(self, remote_dir: str):
        """递归创建远程目录"""
        dirs = []
        d = remote_dir
        while d and d != "/":
            dirs.append(d)
            d = os.path.dirname(d)
        dirs.reverse()
        for d in dirs:
            try:
                self.sftp.stat(d)
            except FileNotFoundError:
                self.sftp.mkdir(d)

    def upload_string(self, content: str, remote_path: str):
        """将字符串内容写入远程文件"""
        remote_dir = os.path.dirname(remote_path)
        self._sftp_makedirs(remote_dir)
        with self.sftp.file(remote_path, "w") as f:
            f.write(content)

    def upload_file(self, local_path: Path, remote_path: str):
        """上传本地文件到远程"""
        remote_dir = os.path.dirname(remote_path)
        self._sftp_makedirs(remote_dir)
        self.sftp.put(str(local_path), remote_path)

    # ---- 配置生成 ----

    def generate_config_yaml(self) -> str:
        config = copy.deepcopy(self.base_config)
        if self.config_overrides:
            config = deep_merge(config, self.config_overrides)
        return yaml.dump(config, default_flow_style=False, allow_unicode=True, sort_keys=False)

    def generate_env(self) -> str:
        env = copy.deepcopy(self.common_env)
        # 默认 OBJECTSTORE_PREFIX 为服务器名
        if "OBJECTSTORE_PREFIX" not in env:
            env["OBJECTSTORE_PREFIX"] = self.name
        # 强制指定管理中心前端页面的路径，防止对象存储模式下路径漂移导致旧页面缓存
        if "MANAGEMENT_STATIC_PATH" not in env:
            env["MANAGEMENT_STATIC_PATH"] = "/CLIProxyAPI/static/management.html"
        env.update(self.env_overrides)
        lines = [f"{k}={v}" for k, v in env.items()]
        return "\n".join(lines) + "\n"

    def generate_compose(self) -> str:
        with open(BASE_COMPOSE, "r", encoding="utf-8") as f:
            compose = yaml.safe_load(f)

        svc = compose.get("services", {}).get("cli-proxy-api", {})
        # 自定义端口
        if self.exposed_ports:
            svc["ports"] = self.exposed_ports
        # 使用 .env 文件
        svc["env_file"] = [".env"]
        svc.pop("environment", None)
        # 固定 volume 映射
        svc["volumes"] = [
            "./config.yaml:/CLIProxyAPI/config.yaml",
            "./auths:/root/.cli-proxy-api",
            "./logs:/CLIProxyAPI/logs",
            "./static:/CLIProxyAPI/static",
            "./data:/data",
        ]

        if self.build_from_source:
            # 从源码构建：设置 build context 为当前目录
            svc["image"] = "cli-proxy-api:local"
            svc["build"] = {"context": ".", "dockerfile": "Dockerfile"}
            svc.pop("pull_policy", None)
        else:
            svc["image"] = self.docker_image
            svc.pop("build", None)
            svc["pull_policy"] = "always"

        return yaml.dump(compose, default_flow_style=False, allow_unicode=True, sort_keys=False)

    # ---- 初始化 ----

    def init_server(self) -> dict:
        result = {"name": self.name, "host": self.host, "success": False, "error": None}
        try:
            self.connect()
            if not INIT_SCRIPT.exists():
                result["error"] = f"初始化脚本不存在: {INIT_SCRIPT}"
                return result

            remote_script = "/tmp/cliproxy_init.sh"
            log("上传初始化脚本...", "UPLOAD", self.name)
            self.upload_file(INIT_SCRIPT, remote_script)
            self.exec_cmd(f"chmod +x {remote_script}")

            log("执行初始化脚本（可能需要几分钟）...", "INFO", self.name)
            code, out, err = self.exec_cmd(f"bash {remote_script} {self.deploy_path}", timeout=300)
            print(out)
            if code != 0:
                result["error"] = f"初始化失败 (exit={code}): {err}"
                return result

            self.exec_cmd(f"rm -f {remote_script}")
            result["success"] = True
            log("初始化完成", "OK", self.name)
        except Exception as e:
            result["error"] = str(e)
            log(f"初始化失败: {e}", "ERROR", self.name)
        finally:
            self.disconnect()
        return result

    # ---- 核心部署 ----

    def deploy(self, config_only: bool = False) -> dict:
        result = {"name": self.name, "host": self.host, "success": False,
                  "error": None, "health": None}
        try:
            self.connect()

            # 检查 Docker
            code, _, _ = self.exec_cmd("docker --version")
            if code != 0:
                log("Docker 未安装，自动执行初始化...", "WARN", self.name)
                self.disconnect()
                init_result = self.init_server()
                if not init_result["success"]:
                    result["error"] = f"自动初始化失败: {init_result['error']}"
                    return result
                self.connect()

            # 创建远程目录
            self.exec_cmd(f"mkdir -p {self.deploy_path}/{{auths,logs,static}}")
            self.exec_cmd(f"mkdir -p /data/cliproxy")

            # 始终同步最新的前端页面，无论是否 build_from_source，确保用户能看到最新的 UI
            self._upload_frontend_asset()

            if not config_only and self.build_from_source:
                # 从源码构建：先上传源码（会覆盖 deploy_path 下的文件）
                log("停止旧容器...", "DOCKER", self.name)
                self.exec_cmd(f"cd {self.deploy_path} && docker compose down 2>/dev/null || true", timeout=60)
                log("从源码构建（首次可能需要5-10分钟）...", "DOCKER", self.name)
                self._upload_source()

            # 上传生成的配置文件（放在源码上传之后，确保不被覆盖）
            log("上传 config.yaml", "UPLOAD", self.name)
            self.upload_string(self.generate_config_yaml(), f"{self.deploy_path}/config.yaml")

            log("上传 .env", "UPLOAD", self.name)
            self.upload_string(self.generate_env(), f"{self.deploy_path}/.env")

            log("上传 docker-compose.yml", "UPLOAD", self.name)
            self.upload_string(self.generate_compose(), f"{self.deploy_path}/docker-compose.yml")

            if config_only:
                # 仅更新配置 → down + up 确保 .env 重新加载
                log("重启容器...", "DOCKER", self.name)
                self.exec_cmd(f"cd {self.deploy_path} && docker compose down", timeout=60)
                code, out, err = self.exec_cmd(
                    f"cd {self.deploy_path} && docker compose up -d", timeout=120)
                if code != 0:
                    result["error"] = f"重启失败: {err}"
                    return result
            else:
                # 完整部署
                if self.build_from_source:
                    build_cmd = (
                        f"cd {self.deploy_path} && docker compose build --no-cache "
                        f"--build-arg VERSION={self.version_info['version']} "
                        f"--build-arg COMMIT={self.version_info['commit']} "
                        f"--build-arg BUILD_DATE={self.version_info['build_date']}"
                    )
                    code, out, err = self.exec_cmd(build_cmd, timeout=900)
                    if code != 0:
                        result["error"] = f"构建失败: {err[:300]}"
                        return result
                else:
                    log(f"拉取镜像 {self.docker_image}", "DOCKER", self.name)
                    code, out, err = self.exec_cmd(
                        f"cd {self.deploy_path} && docker compose pull", timeout=300)
                    if code != 0:
                        result["error"] = f"拉取镜像失败: {err[:300]}"
                        return result

                log("启动容器...", "DOCKER", self.name)
                code, out, err = self.exec_cmd(
                    f"cd {self.deploy_path} && docker compose up -d --remove-orphans", timeout=120)
                if code != 0:
                    result["error"] = f"启动失败: {err[:300]}"
                    return result

            # 健康检查
            result["health"] = self._health_check()
            result["success"] = True
            log("部署成功", "OK", self.name)

        except paramiko.AuthenticationException:
            result["error"] = "SSH 认证失败，请检查密码或密钥"
            log(result["error"], "ERROR", self.name)
        except paramiko.SSHException as e:
            result["error"] = f"SSH 错误: {e}"
            log(result["error"], "ERROR", self.name)
        except TimeoutError:
            result["error"] = "SSH 连接超时"
            log(result["error"], "ERROR", self.name)
        except Exception as e:
            result["error"] = str(e)
            log(f"部署失败: {e}", "ERROR", self.name)
        finally:
            self.disconnect()
        return result

    def _upload_frontend_asset(self):
        """同步本地最新构建的 frontend dist 文件到后端 static 目录并上传"""
        log("同步前端页面...", "UPLOAD", self.name)
        frontend_dist = PROJECT_ROOT.parent / "Cli-Proxy-API-Management-Center" / "dist" / "index.html"
        target_html = PROJECT_ROOT / "static" / "management.html"
        
        # 1. 拷贝到本地 backend static
        if frontend_dist.exists():
            import shutil
            try:
                target_html.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(frontend_dist, target_html)
            except Exception as e:
                log(f"同步本地前端页面失败: {e}", "WARN", self.name)
        
        # 2. 上传到远程 VPS 的 static
        if target_html.exists():
            remote_html = f"{self.deploy_path}/static/management.html"
            try:
                self.upload_file(target_html, remote_html)
                log("前端页面上传成功", "OK", self.name)
            except Exception as e:
                log(f"上传前端页面失败: {e}", "WARN", self.name)
        else:
            log(f"前端产物不存在，跳过同步: {target_html}", "WARN", self.name)

    def _upload_source(self):
        """打包并上传源码到服务器（直接解压到 deploy_path）"""
        log("打包源码...", "UPLOAD", self.name)
        
        # 写入 .build-info 文件
        build_info_path = PROJECT_ROOT / ".build-info"
        try:
            with open(build_info_path, "w", encoding="utf-8") as f:
                f.write(f"BUILD_INFO_VERSION={self.version_info['version']}\n")
                f.write(f"BUILD_INFO_COMMIT={self.version_info['commit']}\n")
                f.write(f"BUILD_INFO_DATE={self.version_info['build_date']}\n")
        except Exception as e:
            log(f"写入 .build-info 失败: {e}", "WARN", self.name)

        zip_path = Path(tempfile.gettempdir()) / f"cliproxy_src_{self.name}.zip"
        file_count = 0
        with zipfile.ZipFile(zip_path, "w", zipfile.ZIP_DEFLATED) as zf:
            for root, dirs, files in os.walk(PROJECT_ROOT):
                dirs[:] = [d for d in dirs if not _should_exclude(d)]
                for fname in files:
                    fp = Path(root) / fname
                    rel = fp.relative_to(PROJECT_ROOT)
                    if not _should_exclude(str(rel)):
                        zf.write(fp, rel)
                        file_count += 1
        size_mb = zip_path.stat().st_size / 1024 / 1024
        log(f"打包完成: {file_count} 文件, {size_mb:.1f}MB", "OK", self.name)

        remote_zip = "/tmp/cliproxy_src.zip"
        log(f"上传源码包 ({size_mb:.1f}MB)...", "UPLOAD", self.name)
        self.upload_file(zip_path, remote_zip)
        # 解压到 deploy_path（Dockerfile、go.mod 等在根目录）
        log("解压源码...", "UPLOAD", self.name)
        self.exec_cmd(f"cd {self.deploy_path} && unzip -o {remote_zip}", timeout=60)
        self.exec_cmd(f"rm -f {remote_zip}")
        
        # 清理本地的 .build-info
        if build_info_path.exists():
            build_info_path.unlink(missing_ok=True)
        zip_path.unlink(missing_ok=True)
        # Fix .dockerignore: strip CRLF + remove config.yaml exclusion for Docker build
        fix_cmd = (
            "cd {dp} && tr -d '\r' < .dockerignore "
            "| grep -v '^config\.yaml$' > /tmp/_di_fix "
            "&& mv /tmp/_di_fix {dp}/.dockerignore"
        ).format(dp=self.deploy_path)
        self.exec_cmd(fix_cmd)

    # ---- 健康检查 ----

    def _health_check(self) -> dict:
        retries = self.hc_config.get("retries", 5)
        interval = self.hc_config.get("interval", 3)
        api_port = self.hc_config.get("api_port", 8317)

        health = {"running": False, "api_ok": False, "errors": []}
        log(f"健康检查 (最多 {retries} 次)...", "CHECK", self.name)

        for i in range(retries):
            time.sleep(interval)

            # 检查容器状态
            code, out, _ = self.exec_cmd("docker ps --filter name=cli-proxy-api --format '{{.Status}}'")
            if code == 0 and "Up" in out:
                health["running"] = True
            else:
                continue

            # 检查 API
            code, out, _ = self.exec_cmd(
                f"curl -s -o /dev/null -w '%{{http_code}}' http://localhost:{api_port}/v1/models",
                timeout=10)
            if code == 0 and out.strip("'\" ") == "200":
                health["api_ok"] = True
                log(f"健康检查通过 (第{i+1}次)", "OK", self.name)
                break
            log(f"健康检查第{i+1}次: 容器={'运行中' if health['running'] else '未运行'}, API={out}", "WARN", self.name)

        # 检查日志错误
        if health["running"]:
            code, out, _ = self.exec_cmd("docker logs cli-proxy-api --tail 30 2>&1 | grep -iE 'panic|fatal' || true")
            if out.strip():
                health["errors"] = out.strip().split("\n")[:5]
                log(f"日志中发现错误: {len(health['errors'])} 条", "WARN", self.name)

        return health

    # ---- 状态查询 ----

    def get_status(self) -> dict:
        status = {"name": self.name, "host": self.host, "success": False,
                  "error": None, "container": None, "uptime": None, "api_ok": False}
        try:
            self.connect()

            # 容器状态
            code, out, _ = self.exec_cmd(
                "docker ps --filter name=cli-proxy-api --format '{{.Status}} | {{.Ports}}'")
            status["container"] = out if out else "未运行"

            # 系统 uptime
            _, out, _ = self.exec_cmd("uptime -p 2>/dev/null || uptime")
            status["uptime"] = out

            # API 检查
            api_port = self.hc_config.get("api_port", 8317)
            code, out, _ = self.exec_cmd(
                f"curl -s -o /dev/null -w '%{{http_code}}' http://localhost:{api_port}/v1/models",
                timeout=5)
            status["api_ok"] = (code == 0 and out.strip("'\" ") == "200")

            # 磁盘
            _, out, _ = self.exec_cmd("df -h / | tail -1 | awk '{print $3\"/\"$2\" (\"$5\")\"}'")
            status["disk"] = out

            status["success"] = True
        except Exception as e:
            status["error"] = str(e)
        finally:
            self.disconnect()
        return status


def _should_exclude(path: str) -> bool:
    parts = Path(path).parts
    for pat in EXCLUDE_PATTERNS:
        if fnmatch(path, pat):
            return True
        for part in parts:
            if fnmatch(part, pat):
                return True
    return False


# ==================== 并行部署 ====================

def run_parallel(func, servers: list, config: dict, base_cfg: dict, version_info: dict,
                 parallel: int, **kwargs) -> list:
    results = []
    with ThreadPoolExecutor(max_workers=parallel) as pool:
        futures = {}
        for srv in servers:
            deployer = VPSDeployer(srv, config, base_cfg, version_info)
            futures[pool.submit(func, deployer, **kwargs)] = srv
        for future in as_completed(futures):
            results.append(future.result())
    return results


def _do_deploy(deployer: VPSDeployer, config_only=False):
    return deployer.deploy(config_only=config_only)

def _do_init(deployer: VPSDeployer):
    return deployer.init_server()

def _do_status(deployer: VPSDeployer):
    return deployer.get_status()

def _do_health(deployer: VPSDeployer):
    result = {"name": deployer.name, "host": deployer.host, "success": False, "error": None, "health": None}
    try:
        deployer.connect()
        result["health"] = deployer._health_check()
        result["success"] = True
    except Exception as e:
        result["error"] = str(e)
    finally:
        deployer.disconnect()
    return result


# ==================== 汇总打印 ====================

def print_summary(results: list, action: str = "部署"):
    print(f"\n{'=' * 65}")
    print(f"  {action}汇总")
    print(f"{'=' * 65}")

    ok = sum(1 for r in results if r.get("success"))
    fail = len(results) - ok

    for r in results:
        icon = "✅" if r.get("success") else "❌"
        extra = ""
        if r.get("health"):
            h = r["health"]
            extra = f" | API={'✅' if h.get('api_ok') else '❌'}"
        if r.get("container"):
            extra = f" | {r['container']}"
        if r.get("error"):
            extra = f" | {r['error'][:60]}"
        print(f"  {icon} {r['name']:12s} ({r['host']:>15s}){extra}")

    print(f"\n  成功: {ok}  失败: {fail}")
    print(f"{'=' * 65}\n")


# ==================== 主入口 ====================

def main():
    parser = argparse.ArgumentParser(description="CLIProxyAPI VPS 自动化部署工具")
    parser.add_argument("--servers", type=str, help="指定服务器名称(逗号分隔)")
    parser.add_argument("--parallel", type=int, help="并行数量")
    parser.add_argument("--config-only", action="store_true", help="仅更新配置+重启")
    parser.add_argument("--init", type=str, nargs="?", const="__ALL__", help="初始化服务器(不指定则全部)")
    parser.add_argument("--status", action="store_true", help="查看服务器状态")
    parser.add_argument("--health", action="store_true", help="仅执行健康检查")
    parser.add_argument("--config", type=str, help="配置文件路径")
    args = parser.parse_args()

    print("=" * 65)
    print("  CLIProxyAPI VPS 自动化部署工具")
    print("=" * 65)

    # 加载配置
    config_path = Path(args.config) if args.config else None
    config = load_config(config_path)
    base_cfg = load_base_config()

    servers = config.get("servers", [])
    if not servers:
        log("未配置任何服务器，请编辑 vps_deploy_config.yaml", "ERROR")
        sys.exit(1)

    # 筛选服务器
    target_names = None
    if args.servers:
        target_names = [s.strip() for s in args.servers.split(",")]
    elif args.init and args.init != "__ALL__":
        target_names = [s.strip() for s in args.init.split(",")]

    if target_names:
        servers = [s for s in servers if s["name"] in target_names]
        not_found = set(target_names) - {s["name"] for s in servers}
        if not_found:
            log(f"未找到服务器: {', '.join(not_found)}", "WARN")

    if not servers:
        log("没有匹配的服务器", "ERROR")
        sys.exit(1)

    parallel = args.parallel or config.get("parallel_count", 3)

    # 获取本地版本信息
    version_info = get_local_version_info()

    # 执行操作
    if args.init is not None:
        log(f"开始初始化 {len(servers)} 台服务器", "INFO")
        results = run_parallel(_do_init, servers, config, base_cfg, version_info, parallel)
        print_summary(results, "初始化")

    elif args.status:
        log(f"查询 {len(servers)} 台服务器状态", "INFO")
        results = run_parallel(_do_status, servers, config, base_cfg, version_info, parallel)
        print_summary(results, "状态")

    elif args.health:
        log(f"健康检查 {len(servers)} 台服务器", "INFO")
        results = run_parallel(_do_health, servers, config, base_cfg, version_info, parallel)
        print_summary(results, "健康检查")

    else:
        action = "配置更新" if args.config_only else "部署"
        log(f"开始{action}到 {len(servers)} 台服务器 (并行: {parallel})", "UPLOAD")
        results = run_parallel(_do_deploy, servers, config, base_cfg, version_info, parallel,
                               config_only=args.config_only)
        print_summary(results, action)


if __name__ == "__main__":
    main()
