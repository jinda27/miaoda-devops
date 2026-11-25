#!/bin/bash

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
out="$root/packages/windows/x64/standard"
portable_runtime=""
bundle_flags=()
# 便携构建使用已解包的 Windows x64 依赖目录。
if [[ $# -gt 0 ]]; then
  if [[ $# -ne 2 || "$1" != "--portable-runtime" || ! -d "$2" ]]; then
    printf '%s\n' "用法：$0 [--portable-runtime <Windows 依赖目录>]" >&2
    exit 1
  fi
  portable_runtime="$(cd "$2" && pwd -P)"
  out="$root/packages/windows/x64"
  bundle_flags=(-compress)
  # 单个依赖缺失时停止构建，避免发布不完整的便携包。
  for item in node/node.exe node/node_modules/npm/bin/npm-cli.js cli/lark-cli.exe \
    git/cmd/git.exe git/bin/bash.exe git/usr/bin/msys-2.0.dll git/usr/bin/perl.exe \
    zip/usr/bin/zip.exe; do
    if [[ ! -s "$portable_runtime/$item" ]]; then
      printf '便携依赖缺少文件：%s\n' "$item" >&2
      exit 1
    fi
  done
fi
# 按北京时间命名，同一天使用同一文件名。
build_date="$(TZ=Asia/Shanghai date '+%Y%m%d')"
output_name="妙搭部署平台_win_64_${build_date}.zip"
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
mkdir -p "$out"
stage="$(mktemp -d "${TMPDIR:-/tmp}/miaoda-windows-app.XXXXXXXX")"
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/web"
cp -R "$root/fe/dist/." "$stage/web/"
# 同次构建共享标识，防止重复启动时打开旧版本页面。
build_id="$(node -e 'process.stdout.write(require("node:crypto").randomUUID())')"
build_flag="-X app-deployment-platform/internal/startup.BuildID=$build_id"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 "$go_bin" -C "$root/be" \
  build -trimpath -ldflags "$build_flag" -o "$stage/server.exe" ./cmd/server
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 "$go_bin" -C "$root/be" \
  build -trimpath -ldflags "-H=windowsgui $build_flag" -o "$stage/妙搭部署平台.exe" ./cmd/desktop
mkdir -p "$stage/packages"
# 仅复制所需依赖，打包组件直接采用本次源码内置版本。
if [[ -n "$portable_runtime" ]]; then
  mkdir "$stage/runtime"
  for item in node cli git zip; do
    cp -R "$portable_runtime/$item" "$stage/runtime/"
  done
  mkdir "$stage/runtime/assemble"
  tar -xzf "$root/be/internal/environment/infrastructure/assets/assemble.tar.gz" -C "$stage/runtime/assemble"
fi
node --input-type=module - "$stage" "$portable_runtime" <<'NODE'
import fs from 'node:fs'
import path from 'node:path'

const [stage, portable] = process.argv.slice(2)
const start = '完整解压后双击“妙搭部署平台.exe”，浏览器将打开本地工作台。\r\n'
const setup = portable
  ? '请解压到当前用户有写权限的目录，并保留 runtime 目录；依赖随包提供，无需一键安装。\r\n'
  : '缺少依赖时进入初始环境，按系统和架构一键下载并安装。\r\n优先从网络下载，失败后尝试飞书文档；下载包缓存到 packages。\r\n飞书文档下载需要已安装 lark-cli、登录用户有文档访问权限及 docs/drive 授权；也可打开文档手动下载到 packages 后重试。\r\n'
fs.writeFileSync(path.join(stage, '使用说明.txt'), start + setup + '首次使用请点击「初始化飞书」，按页面提示完成配置，再点击「登录飞书」。已有配置可直接复用。\r\n二维码失效时，点击「刷新二维码」后使用新二维码或链接。\r\n飞书授权和妙搭操作仍需访问相应服务。\r\n')
NODE
# 打包成功后替换同名 ZIP，保留中文名称和空缓存目录。
"$go_bin" -C "$root/be" run ./cmd/bundle -source "$stage" -output "$out/$output_name" ${bundle_flags[@]+"${bundle_flags[@]}"}
