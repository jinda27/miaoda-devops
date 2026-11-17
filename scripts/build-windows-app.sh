#!/bin/bash

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
go_bin="$(command -v go || true)"
# 构建时从本机 SDK 查找 Go，成品启动不依赖 Go。
if [[ -z "$go_bin" && -d "$HOME/sdk" ]]; then
  go_bin="$(find "$HOME/sdk" -maxdepth 3 -type f -path '*/bin/go' -perm -111 2>/dev/null | sort | tail -n 1)"
fi
if [[ -z "$go_bin" ]] || ! command -v node >/dev/null || ! command -v npm >/dev/null; then
  printf '%s\n' "构建需要 Node.js、npm 和 Go。" >&2
  exit 1
fi
# 前端仅在构建启动包时编译。
if [[ ! -d "$root/fe/node_modules" ]]; then
  npm --prefix "$root/fe" ci
fi
npm --prefix "$root/fe" run build
mkdir -p "$root/dist"
stage="$(mktemp -d "$root/dist/.windows-app.XXXXXXXX")"
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/web"
cp -R "$root/fe/dist/." "$stage/web/"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 "$go_bin" -C "$root/be" \
  build -trimpath -o "$stage/server.exe" ./cmd/server
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 "$go_bin" -C "$root/be" \
  build -trimpath -ldflags '-H=windowsgui' -o "$stage/妙搭部署平台.exe" ./cmd/desktop
mkdir -p "$stage/packages"
node --input-type=module - "$stage" <<'NODE'
import fs from 'node:fs'
import path from 'node:path'

const [stage] = process.argv.slice(2)
fs.writeFileSync(path.join(stage, '使用说明.txt'), '完整解压后双击“妙搭部署平台.exe”，浏览器将打开本地工作台。\r\n缺少依赖时进入初始环境，按系统和架构一键下载并安装。\r\n优先从网络下载，失败后尝试飞书文档；下载包缓存到 packages。\r\n飞书文档下载需要已安装 lark-cli、登录用户有文档访问权限及 docs/drive 授权；也可打开文档手动下载到 packages 后重试。\r\n平台复用本机 lark-cli 已保存的登录，登录过期时在页面重新授权。\r\n运行无需预装 Node.js、npm 或 Go。飞书授权和妙搭操作仍需访问相应服务。\r\n')
NODE
# 原生 ZIP 打包保留中文名称和空缓存目录。
"$go_bin" -C "$root/be" run ./cmd/bundle -source "$stage" -output "$root/dist/妙搭部署平台-windows-x64.zip"
