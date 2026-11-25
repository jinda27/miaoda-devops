#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PORT="${PORT:-18080}"
GO_BIN="$(command -v go || true)"

# 校验 Node.js 环境。
if ! command -v node >/dev/null 2>&1 || ! command -v npm >/dev/null 2>&1; then
  echo "错误：未找到 Node.js 或 npm。" >&2
  exit 1
fi

# 尝试从本地 SDK 目录查找 Go。
if [[ -z "$GO_BIN" && -d "$HOME/sdk" ]]; then
  GO_BIN="$(find "$HOME/sdk" -maxdepth 3 -type f -path '*/bin/go' -perm -111 2>/dev/null | sort | tail -n 1)"
fi

# 校验 Go 环境。
if [[ -z "$GO_BIN" ]]; then
  echo "错误：未找到 Go，请安装 Go 1.22 或更高版本。" >&2
  exit 1
fi

# 首次启动时安装前端依赖。
if [[ ! -d "$ROOT_DIR/fe/node_modules" ]]; then
  npm --prefix "$ROOT_DIR/fe" ci
fi

echo "构建前端..."
npm --prefix "$ROOT_DIR/fe" run build

source "$ROOT_DIR/scripts/port-guard.sh"
release_port "$PORT" confirm_port_in_terminal || {
  result=$?
  if [[ "$result" -eq 2 ]]; then
    echo "已取消启动。"
    exit 0
  fi
  exit "$result"
}

echo "启动服务：http://127.0.0.1:$PORT/"
export PORT
exec "$GO_BIN" -C "$ROOT_DIR/be" run ./cmd/server
