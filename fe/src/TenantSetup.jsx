import { useEffect, useState } from 'react'
import { api } from './api.js'
import { listenForTask } from './taskPolling.js'

export default function TenantSetup({ installed, firstRun, disabled, refresh, setSelected, onConfirmAccount, onBusyChange }) {
  const [open, setOpen] = useState(false)
  const [name, setName] = useState('')
  const [task, setTask] = useState(null)
  const [creating, setCreating] = useState(false)
  const [cancelling, setCancelling] = useState(false)
  const [refreshingQR, setRefreshingQR] = useState(false)
  const [error, setError] = useState('')
  const [message, setMessage] = useState('')
  const pending = task?.status === 'pending'
  const expired = task?.status === 'expired'
  const initial = task?.initial

  // 页面重载后继续展示后台配置链接。
  useEffect(() => {
    if (!installed) return
    const controller = new AbortController()
    api('tenants', { signal: controller.signal }).then((result) => {
      if (!controller.signal.aborted && ['pending', 'expired'].includes(result.status)) {
        setName(result.name)
        setTask(result)
        setOpen(true)
      }
    }).catch((err) => {
      if (!controller.signal.aborted) setError(err.message)
    })
    return () => controller.abort()
  }, [installed])

  useEffect(() => {
    onBusyChange(open || creating || pending)
    return () => onBusyChange(false)
  }, [open, creating, pending, onBusyChange])

  // 配置完成后刷新并选中新租户，保留登录授权为下一步。
  useEffect(() => {
    if (!pending || refreshingQR) return
    let refreshAttempts = 0
    function finish(name) {
      setSelected(name)
      setTask(null)
      setCancelling(false)
      setOpen(false)
      setName('')
      setError('')
      setMessage('飞书配置已就绪，请点击「登录飞书」完成授权。')
    }
    return listenForTask(async (signal) => {
      try {
        const result = await api('tenants/' + task.id, { signal })
        if (signal.aborted) return
        if (result.status === 'pending') {
          setTask(result)
          setError('')
          return 2000
        }
        if (result.status === 'expired') {
          setTask(result)
          setCancelling(false)
          return
        }
        // 新配置落盘后直接更新选中项并收起二维码。
        if (result.status === 'done' && result.account?.name === task.name) {
          onConfirmAccount(result.account)
          finish(task.name)
          return
        }
        const next = await refresh()
        if (signal.aborted) return
        const profileFound = next?.profiles.some((profile) => profile.name === task.name)
        // 配置完成后允许短暂的读取失败，自动重试而不重复创建。
        if (result.status === 'done' && !profileFound && ++refreshAttempts < 3) {
          return 1200
        }
        setTask(null)
        setCancelling(false)
        if (result.status === 'done' && profileFound) {
          finish(task.name)
        } else if (result.status === 'cancelled') {
          setOpen(false)
          setMessage(result.message)
        } else {
          setOpen(!task.initial)
          setError(result.message || '暂未读取到已完成的配置，请重试连接。')
        }
      } catch (err) {
        if (!signal.aborted) {
          setError(err.message)
          // 后台重启会使原任务失效，此时允许重新创建。
          if (err.status === 404) {
            setTask(null)
            setCancelling(false)
            setOpen(!task.initial)
            void refresh()
            return
          }
          return 3000
        }
      }
    })
  }, [pending, task?.id, task?.name, task?.initial, refreshingQR, refresh, setSelected, onConfirmAccount])

  async function createTenant(event) {
    event.preventDefault()
    await startConfiguration(false)
  }

  async function startConfiguration(initialize) {
    setError('')
    setMessage('')
    setCreating(true)
    try {
      await showConfiguration(await requestConfiguration(initialize, name))
    } catch (err) {
      setError(err.message)
    } finally {
      setCreating(false)
    }
  }

  function requestConfiguration(initialize, profile) {
    return initialize
      ? api('tenants/initialize', { data: {} })
      : api('tenants', { data: { name: profile.trim() } })
  }

  async function showConfiguration(result) {
    // 配置恰好已完成时直接加载账号，不再显示旧码。
    if (result.status === 'existing') {
      const next = await refresh()
      if (next?.profiles.some((profile) => profile.name === result.name)) setSelected(result.name)
      setTask(null)
      setOpen(false)
      return
    }
    setName(result.name)
    setTask(result)
    setOpen(true)
  }

  async function refreshConfigQR() {
    setError('')
    setRefreshingQR(true)
    try {
      let result
      try {
        result = await api('tenants/' + task.id + '/refresh', { data: {} })
      } catch (err) {
        // 本地服务重启后使用同一种配置方式重新发起。
        if (err.status !== 404) throw err
        result = await requestConfiguration(task.initial, task.name)
      }
      await showConfiguration(result)
    } catch (err) {
      setTask((current) => ({ ...current, status: 'expired', url: '', qr: '' }))
      setError(err.message)
    } finally {
      setRefreshingQR(false)
    }
  }

  async function cancel() {
    // 尚未发起配置时直接收起表单。
    if (!task) {
      setTask(null)
      setOpen(false)
      setError('')
      return
    }
    setCancelling(true)
    setError('')
    try {
      await api('tenants/' + task.id + '/cancel', { data: {} })
      if (expired) {
        setTask(null)
        setOpen(false)
        setCancelling(false)
      }
    } catch (err) {
      setCancelling(false)
      setError(err.message)
    }
  }

  return (
    <div className="tenant-setup">
      <div className="section-heading">
        <h2>租户账号</h2>
        {!open && !firstRun && <button className="button secondary" disabled={!installed || disabled} onClick={() => {
          setOpen(true)
          setError('')
          setMessage('')
        }}>添加租户</button>}
      </div>
      {firstRun && !open && (
        <div className="tenant-form">
          <p className="muted">首次使用需在飞书确认配置，完成后即可登录。</p>
          <div className="button-row">
            <button className="button primary" disabled={disabled || creating} onClick={() => startConfiguration(true)}>
              {creating ? '正在发起…' : error ? '重试初始化' : '初始化飞书'}
            </button>
          </div>
        </div>
      )}
      {open && (
        <form className="tenant-form" onSubmit={createTenant}>
          {!initial && <label className="field">
            配置名称
            <input value={name} maxLength={64} required autoFocus disabled={creating || pending || expired}
              placeholder="例如：my-tenant" onChange={(event) => setName(event.target.value)} />
          </label>}
          {pending || expired ? (
            <div className="auth-details" role="status">
              {refreshingQR ? <span className="muted">正在刷新二维码…</span> : expired ? (
                <p className="notice info">{task.message || '二维码已失效，请刷新二维码。'}</p>
              ) : task.url ? <>
                <a href={task.url} target="_blank" rel="noreferrer">打开飞书租户配置页面</a>
                {task.qr && <img src={task.qr} width="144" height="144" alt="飞书租户配置二维码" />}
                <span className="muted">请完成租户配置，完成后自动刷新账号列表。</span>
              </> : <span className="muted">正在生成租户配置链接…</span>}
            </div>
          ) : <p className="muted">输入名称后创建配置，再打开链接或扫码完成。</p>}
          <div className="button-row">
            {(pending || expired) && <button type="button" className={'button ' + (expired ? 'primary' : 'secondary')}
              disabled={refreshingQR || cancelling} onClick={refreshConfigQR}>
              {refreshingQR ? '正在刷新…' : '刷新二维码'}
            </button>}
            {!pending && !expired && <button className="button primary" type="submit" disabled={creating || !name.trim()}>
              {creating ? '正在创建…' : '创建配置'}
            </button>}
            <button className="button secondary" type="button" disabled={creating || cancelling || refreshingQR} onClick={cancel}>
              {cancelling ? '正在取消…' : '取消'}
            </button>
          </div>
        </form>
      )}
      {error && <p className="notice danger" role="alert">{error}</p>}
      {message && <p className="notice info" role="status">{message}</p>}
    </div>
  )
}
