#!/bin/bash

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
if [[ "$(uname -s)" != Darwin ]]; then
  printf '%s\n' "请在 macOS 上构建应用。" >&2
  exit 1
fi

go_bin="$(command -v go || true)"
if [[ -z "$go_bin" && -d "$HOME/sdk" ]]; then
  go_bin="$(find "$HOME/sdk" -maxdepth 3 -type f -path '*/bin/go' -perm -111 2>/dev/null | sort | tail -n 1)"
fi
if [[ -z "$go_bin" ]] || ! command -v node >/dev/null || ! command -v npm >/dev/null; then
  printf '%s\n' "构建需要 Node.js、npm 和 Go。" >&2
  exit 1
fi

# 按当前 Mac 架构构建后端。
case "$(uname -m)" in
  arm64) arch=arm64 ;;
  x86_64) arch=amd64 ;;
  *) printf '%s\n' "当前 Mac 架构暂不支持。" >&2; exit 1 ;;
esac

if [[ ! -d "$root/fe/node_modules" ]]; then
  npm --prefix "$root/fe" ci
fi
npm --prefix "$root/fe" run build

out="$root/packages/mac"
name="妙搭部署平台"
mkdir -p "$out"
stage="$(mktemp -d "$out/.macos-app.XXXXXXXX")"
trap 'rm -rf "$stage"' EXIT
bundle="$stage/$name.app"
contents="$bundle/Contents"
mkdir -p "$contents/MacOS" "$contents/Resources/web"
install -m 0755 "$root/scripts/macos-app-launcher.sh" "$contents/MacOS/launcher"
install -m 0644 "$root/scripts/port-guard.sh" "$contents/Resources/port-guard.sh"
cp -R "$root/fe/dist/." "$contents/Resources/web/"
printf '%s\n' "$PATH" > "$contents/Resources/build-path"

# 安装清单及小型打包组件由服务内置，构建无需预存安装包。
CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" "$go_bin" -C "$root/be" \
  build -trimpath -o "$contents/MacOS/server" ./cmd/server

cat > "$contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleIdentifier</key>
  <string>local.miaoda.deployment-platform</string>
  <key>CFBundleName</key>
  <string>妙搭部署平台</string>
  <key>CFBundleDisplayName</key>
  <string>妙搭部署平台</string>
  <key>CFBundleExecutable</key>
  <string>launcher</string>
  <key>CFBundlePackageType</key>
  <string>APPL</string>
  <key>CFBundleVersion</key>
  <string>1</string>
  <key>CFBundleShortVersionString</key>
  <string>0.1.0</string>
  <key>LSUIElement</key>
  <true/>
</dict>
</plist>
PLIST

/usr/bin/plutil -lint "$contents/Info.plist"
/usr/bin/codesign --force --sign - "$contents/MacOS/server"
/usr/bin/codesign --force --sign - "$bundle"
/usr/bin/codesign --verify --deep --strict "$bundle"

destination="$out/$name.app"
if [[ -e "$destination" ]]; then
  mv "$destination" "$stage/previous.app"
fi
if ! mv "$bundle" "$destination"; then
  if [[ -e "$stage/previous.app" ]]; then
    mv "$stage/previous.app" "$destination"
  fi
  exit 1
fi

printf '已生成 %s\n' "$destination"
