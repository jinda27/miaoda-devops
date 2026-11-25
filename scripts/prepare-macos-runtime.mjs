import fs from 'node:fs'
import { createHash } from 'node:crypto'
import { spawnSync } from 'node:child_process'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const output = process.argv[2]
const architecture = process.arch === 'arm64' ? 'arm64' : 'x64'

// 只在匹配架构的 Mac 上准备可运行依赖。
if (process.platform !== 'darwin' || !['arm64', 'x64'].includes(process.arch) || !output) {
  throw new Error('用法：node scripts/prepare-macos-runtime.mjs <新的运行目录>，需在 Mac 上执行')
}

const destination = path.resolve(output)
const cache = path.join(os.tmpdir(), 'miaoda-macos-downloads', architecture)
const catalog = JSON.parse(fs.readFileSync(path.join(root, 'be/internal/environment/infrastructure/catalog.json'), 'utf8'))

function run(command, args, options = {}) {
  const result = spawnSync(command, args, { stdio: 'inherit', ...options })
  // 下载、解压或运行验证失败时立即停止。
  if (result.error || result.status !== 0) {
    throw result.error || new Error(`${path.basename(command)} 执行失败`)
  }
}

async function digest(filename) {
  const hash = createHash('sha256')
  // 流式核验大文件，避免占用过多内存。
  for await (const chunk of fs.createReadStream(filename)) {
    hash.update(chunk)
  }
  return hash.digest('hex')
}

// 不覆盖现有运行目录，防止准备失败损坏旧成品。
if (fs.existsSync(destination)) {
  throw new Error('运行目录已存在，请使用新的临时目录')
}
fs.mkdirSync(destination, { recursive: true })
fs.mkdirSync(cache, { recursive: true })
const packages = []

// 仅复制官方发行包，不复制本机的登录、配置和缓存。
for (const [component, directory] of [['node', 'node'], ['lark-cli', 'cli'], ['git', 'git']]) {
  const item = catalog.packages.find(pkg => pkg.platform === `mac/${architecture}` && pkg.component === component)
  // 必须有唯一来源和校验值，禁止生成缺少依赖的包。
  if (!item?.source?.startsWith('https://') || !/^[a-f0-9]{64}$/i.test(item.sha256 || '')) {
    throw new Error(`清单缺少 ${component} 的来源或 SHA256`)
  }
  const filename = path.join(cache, path.basename(item.path))
  // 复用已校验的下载缓存，版本变化或文件损坏时重新下载。
  if (!fs.existsSync(filename) || await digest(filename) !== item.sha256.toLowerCase()) {
    const partial = `${filename}.partial`
    process.stdout.write(`下载 ${component} ${item.version}\n`)
    // 中断后保留已下载数据，下次从断点继续。
    run('/usr/bin/curl', ['-q', '--fail', '--location', '--silent', '--show-error',
      '--retry', '2', '--connect-timeout', '20', '--max-time', '240', '--continue-at', '-',
      '--output', partial, item.source])
    // 下载完成且校验通过才替换缓存。
    if (await digest(partial) !== item.sha256.toLowerCase()) {
      fs.rmSync(partial, { force: true })
      throw new Error(`${component} SHA256 校验失败`)
    }
    fs.renameSync(partial, filename)
  }
  const target = path.join(destination, directory)
  fs.mkdirSync(target)
  run('/usr/bin/tar', ['-xzf', filename, '-C', target, ...(component === 'node' ? ['--strip-components=1'] : [])])
  packages.push({ component, version: item.version, sha256: item.sha256, source: item.source })
}

const assemble = path.join(destination, 'assemble')
fs.mkdirSync(assemble)
run('/usr/bin/tar', ['-xzf', path.join(root, 'be/internal/environment/infrastructure/assets/assemble.tar.gz'), '-C', assemble])
fs.writeFileSync(path.join(destination, 'manifest.json'), JSON.stringify({ architecture, packages }, null, 2) + '\n')

// 校验 Git 的联网辅助程序，避免只通过版本检测。
fs.accessSync(path.join(destination, 'git/libexec/git-core/git-remote-https'), fs.constants.X_OK)
const env = {
  ...process.env,
  PATH: `${destination}/cli:${destination}/node/bin:${destination}/git/bin:/usr/bin:/bin:/usr/sbin:/sbin`,
  GIT_EXEC_PATH: path.join(destination, 'git/libexec/git-core'),
  GIT_TEMPLATE_DIR: path.join(destination, 'git/share/git-core/templates'),
}
run(path.join(destination, 'node/bin/node'), ['--version'], { env })
run(path.join(destination, 'node/bin/node'), [path.join(destination, 'node/lib/node_modules/npm/bin/npm-cli.js'), '--version'], { env })
run(path.join(destination, 'cli/lark-cli'), ['--version'], { env })
run(path.join(destination, 'git/bin/git'), ['--version'], { env })
process.stdout.write('Mac 运行依赖准备完成\n')
