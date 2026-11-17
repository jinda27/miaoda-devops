import { useEffect, useState } from 'react'
import { api } from './api.js'

const states = {
  ready: { label: '已就绪', className: 'success', icon: '✓' },
  missing: { label: '未安装', className: 'warning', icon: '↓' },
  version_mismatch: { label: '不可用', className: 'failed', icon: '!' },
}
const stepStates = { pending: '等待安装', running: '正在安装', success: '已完成', failed: '安装失败' }

export default function EnvironmentSetup({ report, checking, error, onCheck, onComplete, onContinue }) {
  const [installation, setInstallation] = useState(report?.installation || null)
  const [starting, setStarting] = useState(false)
  const [installError, setInstallError] = useState('')
  const running = installation?.status === 'running'
  const busy = running || starting
  const items = report?.items || []
  const missing = items.filter((item) => item.status !== 'ready').length
  const installable = items.some((item) => item.status !== 'ready' && item.packageAvailable)
  const system = report?.platform === 'darwin' ? 'macOS' : report?.platform === 'windows' ? 'Windows' : report?.platform
  const architecture = report?.architecture === 'amd64' ? 'x64' : report?.architecture

  // 页面重载时恢复服务端仍在运行的安装任务。
  useEffect(() => {
    if (report?.installation) setInstallation(report.installation)
  }, [report?.installation])

  useEffect(() => {
    if (!running) return
    const controller = new AbortController()
    let timer
    // 只轮询轻量进度接口，结束时再执行依赖复检。
    async function poll() {
      try {
        const result = await api('environment/install', { signal: controller.signal })
        if (controller.signal.aborted) return
        if (!result) throw new Error('安装任务已中断，请重新检测后重试。')
        setInstallation(result)
        setInstallError('')
        if (result.status === 'running') {
          timer = setTimeout(poll, 700)
          return
        }
        await onComplete(result.status === 'success')
      } catch (err) {
        if (controller.signal.aborted) return
        setInstallError(err.message)
        timer = setTimeout(poll, 2000)
      }
    }
    timer = setTimeout(poll, 350)
    return () => {
      controller.abort()
      clearTimeout(timer)
    }
  }, [running, onComplete])

  async function install() {
    setStarting(true)
    setInstallError('')
    try {
      setInstallation(await api('environment/install', { data: {} }))
    } catch (err) {
      setInstallError(err.message)
    } finally {
      setStarting(false)
    }
  }

  return (
    <section className="workspace environment-workspace">
      <div className="page-heading">
        <h1>初始环境</h1>
        <p>检查本机环境，准备应用打包和部署所需的工具。</p>
      </div>
      <ol className="setup-stages" aria-label="初始化步骤">
        <li aria-current="step"><span>1</span>初始环境</li>
        <li><span>2</span>账号连接</li>
      </ol>
      {(error || installError) && <p className="notice danger environment-notice" role="alert">{error || installError}</p>}
      {report && !report.supported && (
        <p className="notice danger environment-notice" role="alert">当前仅支持 Mac（Apple Silicon / Intel）和 Windows x64。</p>
      )}
      <div className="panel environment-panel" aria-busy={checking}>
        <div className="panel-header">
          <div className="environment-panel-title">
            <h2>依赖检查</h2>
            {system && <span className="environment-platform">{system} · {architecture}</span>}
          </div>
          <button className="button secondary" disabled={checking || busy} onClick={onCheck}>
            {checking ? '正在检测…' : '重新检测'}
          </button>
        </div>
        {report ? (
          <div className="environment-table-wrap">
            <table className="environment-table">
              <thead><tr><th>依赖</th><th>版本要求</th><th>当前版本</th><th>状态</th></tr></thead>
              <tbody>
                {items.map((item) => {
                  const state = states[item.status] || states.missing
                  return (
                    <tr key={item.id}>
                      <td><strong>{item.name}</strong></td>
                      <td className="muted">{item.requirement}</td>
                      <td>
                        <span>{item.version || '—'}</span>
                        {item.path && <small className="environment-tool-path" title={item.path}>{item.path}</small>}
                      </td>
                      <td>
                        <span className={'pill ' + state.className}><span aria-hidden="true">{state.icon}</span>{state.label}</span>
                        {item.status !== 'ready' && item.message && <small className="environment-item-message">{item.message}</small>}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        ) : (
          <div className="environment-loading" role="status">
            <span className="environment-spinner" aria-hidden="true" />
            {error ? '环境检测未完成，请重新检测。' : '正在检查本机环境…'}
          </div>
        )}
        <div className="environment-actions">
          <div role="status">
            <strong>{busy ? '正在准备环境' : report?.ready ? '环境已就绪' : report ? `${missing} 项依赖需要准备` : '等待检测结果'}</strong>
            <p className="muted">按当前系统和架构下载，网络下载失败后尝试飞书文档，已就绪的依赖自动跳过。</p>
            {report?.documentURL && (
              <a href={report.documentURL} target="_blank" rel="noreferrer">打开下载文档</a>
            )}
          </div>
          {report?.ready && !busy ? (
            <button className="button primary" disabled={checking} onClick={onContinue}>进入账号连接 <span aria-hidden="true">→</span></button>
          ) : (
            <button className="button primary" disabled={checking || busy || !report?.supported || !installable} onClick={install}>
              {busy ? '正在安装…' : installation?.status === 'failed' ? '重试未完成项' : '一键安装'}
            </button>
          )}
        </div>
      </div>
      {installation && (
        <div className="panel environment-progress" role="status" aria-live="polite">
          <div className="panel-header">
            <h2>{running ? '安装进度' : installation.status === 'success' ? '安装完成' : '部分依赖未就绪'}</h2>
            <span className="environment-percent">{installation.percent}%</span>
          </div>
          <div className="environment-progress-bar" role="progressbar" aria-label="环境安装进度"
            aria-valuenow={installation.percent} aria-valuemin={0} aria-valuemax={100}>
            <div style={{ width: `${installation.percent}%` }} />
          </div>
          <ul className="environment-step-list">
            {installation.steps.map((step) => (
              <li key={step.id}>
                <span className={'environment-step-icon ' + step.status} aria-hidden="true">
                  {step.status === 'success' ? '✓' : step.status === 'failed' ? '!' : step.status === 'running' ? '↻' : '·'}
                </span>
                <strong>{step.name}</strong>
                <span className="environment-step-detail">{step.message || step.phase || stepStates[step.status]}</span>
                <span className={'pill ' + (step.status === 'success' ? 'success' : step.status === 'failed' ? 'failed' : step.status === 'running' ? 'running' : '')}>
                  {stepStates[step.status]}
                </span>
              </li>
            ))}
          </ul>
          {!running && <p className="environment-progress-message">{installation.message}</p>}
        </div>
      )}
      {report?.root && <p className="environment-install-location">安装目录 <span>{report.root}</span></p>}
      {report?.packages && (
        <p className="environment-install-location">下载缓存 <span>{report.packages}</span>；手动下载的安装包也可放入此目录后重试。</p>
      )}
    </section>
  )
}
