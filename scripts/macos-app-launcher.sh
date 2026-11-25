#!/bin/bash

set -euo pipefail

bin_dir="$(cd "$(dirname "$0")" && pwd -P)"
resources="$bin_dir/../Resources"
app_parent="$(cd "$bin_dir/../../.." && pwd -P)"
# 在构建目录中启动时保留项目原有的数据和日志位置。
case "$app_parent" in
  */packages/mac) project="${app_parent%/packages/mac}" ;;
  *) project="$(dirname "$app_parent")" ;;
esac
backend="$project/be"
# 独立分发的应用使用用户目录保存任务和日志。
if [[ ! -d "$backend" ]]; then
  project="$HOME/Library/Application Support/MiaodaDeploymentPlatform/workspace"
  mkdir -p "$project" || exit 1
fi

show_error() {
  printf '%s\n' "$1" >&2
  /usr/bin/osascript -e 'on run argv' \
    -e 'display alert "妙搭部署平台" message (item 1 of argv) as critical' \
    -e 'end run' "$1" >/dev/null 2>&1 || true
  exit 1
}

if [[ ! -r "$resources/web/index.html" || ! -x "$bin_dir/server" ]]; then
  show_error "应用文件不完整，请重新复制完整的应用。"
fi

# Finder 启动也优先使用随包依赖，避免依赖构建机的路径。
runtime="$resources/runtime"
export PATH="$runtime/cli:$runtime/node/bin:$runtime/git/bin:/usr/bin:/bin:/usr/sbin:/sbin"
export MIAODA_TOOLS_DIR="$runtime" MIAODA_LARK_CLI="$runtime/cli/lark-cli" MIAODA_ASSEMBLE_SCRIPT=""
# 便携 Git 必须显式定位 HTTPS 辅助程序和初始化模板。
export GIT_EXEC_PATH="$runtime/git/libexec/git-core" GIT_TEMPLATE_DIR="$runtime/git/share/git-core/templates"
export PORT="${PORT:-18080}"
source "$resources/port-guard.sh"

url="http://127.0.0.1:$PORT/"
cd "$project"
log_dir="$project/logs"
mkdir -p "$log_dir" || show_error "无法创建项目的日志目录。"
startup_dir="$(mktemp -d "$log_dir/.macos-startup.XXXXXXXX")" || show_error "无法写入项目的日志目录。"
trap 'rm -rf "$startup_dir"' EXIT
export MIAODA_WEB_DIR="$resources/web" MIAODA_DESKTOP_STARTUP=1 MIAODA_PROJECT_DIR="$project"
# 下载包统一缓存到当前项目的可写目录。
export MIAODA_PACKAGES_DIR="$project/packages"
# 自动结束占用该端口的进程，释放后在原端口启动。
details="$(release_port "$PORT" 2>&1)" || show_error "${details:-无法释放启动端口。}"
/usr/bin/nohup "$bin_dir/server" </dev/null >"$startup_dir/ready" 2>"$startup_dir/stderr" &

# 就绪信号来自本次启动的进程，HTTP 检查确认工作台已开始响应。
for ((attempt = 0; attempt < 50; attempt++)); do
  if /usr/bin/grep -qx 'ready' "$startup_dir/ready" 2>/dev/null &&
      /usr/bin/curl --noproxy '*' -fsS --max-time 1 "${url}api/health" >/dev/null 2>&1 &&
      /usr/bin/curl --noproxy '*' -fsS --max-time 1 "$url" >/dev/null 2>&1; then
    /usr/bin/open "$url" || show_error "服务已启动，请在浏览器打开 $url"
    exit 0
  fi
  /bin/sleep 0.2
done

if [[ -s "$startup_dir/stderr" ]]; then
  details="$(/usr/bin/tail -n 1 "$startup_dir/stderr")"
  show_error "工作台启动失败：$details"
fi
show_error "工作台启动失败，请查看项目中的 logs/server.log。"
