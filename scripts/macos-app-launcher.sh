#!/bin/bash

set -euo pipefail

bin_dir="$(cd "$(dirname "$0")" && pwd -P)"
resources="$bin_dir/../Resources"
project="$(cd "$bin_dir/../../../.." && pwd -P)"
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

confirm_port_in_window() {
  local choice
  choice="$(/usr/bin/osascript -e 'on run argv' \
    -e 'set choice to display alert "端口已被占用" message (item 1 of argv) as warning buttons {"取消", "结束进程并启动"} default button "取消" cancel button "取消"' \
    -e 'return button returned of choice' \
    -e 'end run' "$1" 2>/dev/null)" && [[ "$choice" == "结束进程并启动" ]]
}

if [[ ! -r "$resources/web/index.html" || ! -x "$bin_dir/server" ]]; then
  show_error "应用文件不完整，请重新复制完整的应用。"
fi

# 从 Finder 启动时补充终端安装的 lark-cli、Node.js 和 Git 的搜索路径。
build_path="$(cat "$resources/build-path")"
export PATH="$build_path:$HOME/.local/bin/node/bin:$HOME/.local/bin:$HOME/.npm-global/bin:$HOME/.volta/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
export PORT="${PORT:-18080}"
source "$resources/port-guard.sh"
details="$(release_port_after_confirmation "$PORT" confirm_port_in_window 2>&1)" || {
  result=$?
  [[ "$result" -ne 2 ]] || exit 0
  show_error "${details:-无法检查启动端口。}"
}

url="http://127.0.0.1:$PORT/"
cd "$project"
log_dir="$project/logs"
mkdir -p "$log_dir" || show_error "无法创建项目的日志目录。"
startup_dir="$(mktemp -d "$log_dir/.macos-startup.XXXXXXXX")" || show_error "无法写入项目的日志目录。"
trap 'rm -rf "$startup_dir"' EXIT
export MIAODA_WEB_DIR="$resources/web" MIAODA_DESKTOP_STARTUP=1 MIAODA_PROJECT_DIR="$project"
# 下载包统一缓存到当前项目的可写目录。
export MIAODA_PACKAGES_DIR="$project/packages"
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
