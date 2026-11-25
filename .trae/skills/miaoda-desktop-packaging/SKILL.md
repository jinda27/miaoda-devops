---
name: miaoda-desktop-packaging
description: 从同一份源码生成妙搭部署平台的 Mac 和 Windows 可运行 ZIP。用于给本项目打包、生成安装包或发布桌面使用包。不用于平台内导出某个妙搭业务应用。
---

# 妙搭部署平台双平台打包

每次从同一份源码生成两个 ZIP，直接放在项目的 `packages/` 根目录，不新增平台或架构子目录。

| 平台 | 文件名 |
| --- | --- |
| Mac（Apple 芯片） | `妙搭部署平台_mac_arm64_yyyyMMdd.zip` |
| Windows x64 | `妙搭部署平台_win_64_yyyyMMdd.zip` |

日期按北京时间取年月日，平台标识在日期前，两份成品共用同一日期。同一天重新生成并校验成功后替换同名包。跟随现有文件名，Mac 的 `arm64` 和 Windows 的 `win_64` 不互换。若明确要求 Intel Mac，按实际架构使用 `mac_x64`，不得把 Intel 包标成 `arm64`。

1. 确定源码版本，在 Mac 上用 Node.js 20 和 Go 顺序构建。涉及提交或推送时，从提交快照构建，并额外按 [AGENTS.md](../../../AGENTS.md) 执行离线源码包和飞书附件同步。
2. 运行 `./scripts/build-macos-app.sh`，用 `ditto -c -k --sequesterRsrc --keepParent` 把生成的 `packages/mac/妙搭部署平台.app` 压成 ZIP，保留完整 `.app`。
3. 运行 `./scripts/build-windows-app.sh --portable-runtime <依赖目录>`，其中目录包含 `node/`、`cli/`、`git/`、`zip/`。没有现成目录时，可从可信的旧 Windows 便携包仅解压所需依赖到系统临时目录，或按 `be/internal/environment/infrastructure/catalog.json` 获取并核验官方依赖。只复用运行依赖，每次重新构建平台程序。将生成在 `packages/windows/x64/` 下的本次 ZIP 作为待交付产物。
4. 用 `unzip -tq` 检查两个 ZIP。解压 Mac ZIP 到临时目录，确认后端架构与包名匹配，并用 `codesign --verify --deep --strict` 验证 `.app`；确认 Windows ZIP 包含 x64 的 `妙搭部署平台.exe`、`server.exe`、前端和 `runtime/`。排除凭据、业务数据、日志、缓存和 Git 目录。
5. 两份都通过后，将 ZIP 发布到 `packages/` 根目录。交付两份文件链接；任一构建或校验失败时说明原因，保留旧成品，不把单平台结果算作完成。
