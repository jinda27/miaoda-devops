# 妙搭部署平台

在本机打包妙搭应用，并用安装包在所选飞书租户创建和发布新应用。界面使用 React + Vite，服务使用 Go 标准库。

## 启动

推荐使用预编译启动程序，运行无需预装 Node.js、npm 或 Go：

- Mac：双击 `dist/妙搭部署平台.app`。
- Windows x64：完整解压 `dist/妙搭部署平台-windows-x64.zip`，双击其中的 `妙搭部署平台.exe`。

首次启动会自动检测 Node.js/npm、lark-cli、Git、Bash、zip 和打包组件。检测以命令能否正常运行为准，不强制限定版本；缺少工具或命令不可用时进入登录前的**初始环境**页面，点击「一键安装」按本机操作系统和原生架构下载安装包。优先使用官方网络地址，连接失败、下载中断或校验失败后改从飞书文档附件下载。安装显示进度，失败可重试；安装后自动复检，通过后进入账号连接。侧边栏可随时重新打开初始环境。

下载包缓存到项目的 `packages/`，校验 SHA256 后安装到当前用户的专用目录，已下载且校验通过的包会直接复用。平台启动时自动加入自身命令搜索路径。成品无需安装 Go；小型全栈打包组件内置于服务。版本、来源和校验值见 [`catalog.json`](be/internal/environment/infrastructure/catalog.json)。

文档下载来源：[妙搭应用打包和部署方案 · 第三章安装文件](https://bytedance.larkoffice.com/wiki/Hf8swYthdiavnDkJ8oKcEQAQnCc)。自动下载需已安装 lark-cli，当前 CLI 用户有文档访问权限及 docs/drive 授权；不会继承启动工具的临时令牌。可用 `MIAODA_DOWNLOAD_PROFILE` 指定下载所用的本地租户配置。如果 lark-cli 尚未安装或缺少授权，在初始环境点击「打开下载文档」，手动下载对应包到 `packages/` 根目录后重试，平台会自动识别文件名并校验。文档附件令牌在每次回退时实时读取，不保存临时下载链接。

### 源码启动

需要 Node.js 20.19+ 和 Go 1.22+。在项目根目录运行：

```sh
npm --prefix fe ci
npm --prefix fe start
```

macOS / Linux 可以使用 `./scripts/start.sh`；Windows 可以双击 `scripts\start.bat`，或在项目根目录运行：

```bat
scripts\start.bat
```

启动脚本会首次安装前端依赖、构建前端并启动后端。macOS / Linux 终端启动时，若端口已被占用，会列出进程并等待确认。默认地址为 [http://127.0.0.1:18080/](http://127.0.0.1:18080/)，可通过 `PORT` 修改。Windows CMD 使用 `set PORT=18081`，PowerShell 使用 `$env:PORT = '18081'`。

平台读取 `lark-cli profile list` 中的租户配置，用所选配置登录和执行任务。添加租户配置时，可复制账号连接页面显示的命令到终端执行。每次启动后，需要在账号连接页面使用自己的飞书账号重新授权，完成后才能打包和部署。平台会清除启动工具注入的临时飞书凭证，不会自动沿用已有登录状态。凭据由 lark-cli 管理。

### macOS 双击启动

在 macOS 上运行 `./scripts/build-macos-app.sh`，然后在 Finder 中双击项目内的 `dist/妙搭部署平台.app`。应用包含已构建的前端和后端，启动后会用默认浏览器打开工作台。端口被占用时，弹窗会列出进程名称和 PID。选择「结束进程并启动」后，应用会等待端口释放再启动。选择「取消」会保留原进程。

应用位于项目 `dist/` 时，任务记录和日志继续使用项目的 `data/` 和 `logs/`。单独复制应用到其他目录后，数据保存在 `~/Library/Application Support/MiaodaDeploymentPlatform/workspace`。修改代码后可重新运行构建脚本生成当前 Mac 架构的应用。

### Windows 启动包构建

在配置好 Node.js 和 Go 的 Mac 上运行 `./scripts/build-windows-app.sh`，生成 Windows x64 启动 ZIP。包内包含原生启动器、服务程序、已构建的前端和空 `packages/` 目录，依赖按需下载。Windows 双击启动器后打开本地工作台。端口被其他程序占用时会提示，已运行的平台会直接复用。

## 打包与部署

1. 在**应用打包**页输入应用名称，搜索当前租户可见的全栈应用。通过结果行的「打包」开始，查看各步骤并在完成后下载 ZIP。
2. 在**应用部署**页填写应用名称，选择或拖入 ZIP，再点击「确认部署」。平台优先读取安装包元数据；无清单时会根据工程目录识别应用类型。
3. 在进度面板查看成功、失败、跳过或需检查的步骤。部署完成后，点击面板底部的「打开生产环境」进入已发布的系统，也可以在面板顶部复制访问地址。部署失败且目标应用已创建时，从该任务重试会复用已记录的目标应用 ID。

覆盖已有应用时，填写妙搭应用地址并导入全栈安装包，点击「检查目标应用」。核对应用 ID、当前账号和影响清单，再输入目标应用 ID 并确认。初始化会删除清单中的开发库业务表和枚举、清空生产库业务表数据、删除自定义角色和环境变量，并停用旧自动化。生产表结构随新版本发布更新。清理失败时任务停在「初始化目标应用」。

前端和创意应用使用 lark-cli 源码导出、Git 和妙搭发布命令。全栈打包组件由初始环境安装到用户专用目录，部署由服务端 Go 代码直接完成。也可复用 `~/.agents/skills` 或 `~/.codex/skills` 下的 `miaoda-app-assemble` 脚本，或通过 `MIAODA_ASSEMBLE_SCRIPT` 指定入口路径。全栈包包含源码、配置和数据库结构，源应用业务表数据由妙搭侧单独迁移。

全栈打包脚本需要 `bash`、`node`、`git`、`zip`、`perl` 和基础 Shell 命令；Windows 由便携 Git 提供 Bash 及配套工具。用自定义路径安装 lark-cli 时可设置 `MIAODA_LARK_CLI` 指向可执行文件。`MIAODA_TOOLS_DIR` 可指定专用工具目录，`MIAODA_PACKAGES_DIR` 可指定本地安装包目录。

DDL 按扩展、类型、建表、外键、后置对象、初始化数据的阶段顺序执行，同阶段最多 3 个协程并发处理文件，单文件内 SQL 保持原顺序。任一文件失败会取消同阶段请求并停止后续阶段；建表依赖的函数与序列会提前串行创建。

## 文件与开发

- `data/jobs/` 保存步骤状态、生成的 ZIP 与部署续跑记录。
- `data/imports/` 保存上传的 ZIP 与识别结果。
- `logs/server.log` 保存服务、请求及任务执行日志。服务启动时自动创建文件，重启后继续追加，终端也会显示日志。
- 安装包可能包含妙搭环境变量值。请仅通过受控渠道分享，并妥善保管项目内的 `data/`。
- `fe/src/` 存放界面，`be/` 存放 API 和任务流程。

开发模式运行 `./scripts/dev.sh`，访问 [http://127.0.0.1:5173/](http://127.0.0.1:5173/)。前端修改由 Vite 自动刷新，后端 Go 文件修改后自动编译重启；服务以守护进程运行，不会持续占用当前终端。

```sh
./scripts/dev.sh status
./scripts/dev.sh restart
./scripts/dev.sh stop
```

后端端口可通过 `PORT` 修改，前端端口可通过 `FRONTEND_PORT` 修改。开发日志保存在系统临时目录的 `miaoda-deployment-platform-dev-$UID/` 下。
