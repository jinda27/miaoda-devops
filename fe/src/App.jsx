import { useCallback, useEffect, useState } from 'react'
import { api } from './api.js'
import Workspace from './Workspace.jsx'
import EnvironmentSetup from './EnvironmentSetup.jsx'
import TenantSetup from './TenantSetup.jsx'
import { listenForTask } from './taskPolling.js'

const pages = [
  { id: 'package', label: '应用打包', icon: '↓' },
  { id: 'deploy', label: '应用部署', icon: '↑' },
]

function readPage() {
  const page = location.hash.slice(1)
  return ['deploy', 'environment', 'account'].includes(page) ? page : 'package'
}

export function profileReady(profile) {
  return profile?.user && ['valid', 'needs_refresh', 'expiring'].includes(profile.tokenStatus)
}

export default function App() {
  const [cli, setCLI] = useState(null)
  const [checking, setChecking] = useState(true)
  const [error, setError] = useState('')
  const [selected, setSelected] = useState(() => sessionStorage.getItem('miaoda-profile') || '')
  const [entered, setEntered] = useState(() => sessionStorage.getItem('miaoda-profile') || '')
  const [page, setPage] = useState(readPage)
  const [environment, setEnvironment] = useState(null)
  const [environmentChecking, setEnvironmentChecking] = useState(true)
  const [environmentError, setEnvironmentError] = useState('')

  const refreshEnvironment = useCallback(async () => {
    setEnvironmentChecking(true)
    setEnvironmentError('')
    try {
      const result = await api('environment')
      setEnvironment(result)
      return result
    } catch (err) {
      setEnvironmentError(err.message)
      return null
    } finally {
      setEnvironmentChecking(false)
    }
  }, [])

  const refresh = useCallback(async () => {
    setChecking(true)
    setError('')
    try {
      const result = await api('cli')
      setCLI(result)
      setSelected((current) => {
        if (result.profiles.some((profile) => profile.name === current)) return current
        return result.profiles.find((profile) => profile.active)?.name || result.profiles[0]?.name || ''
      })
      return result
    } catch (err) {
      setError(err.message)
      return null
    } finally {
      setChecking(false)
    }
  }, [])

  const applyConfirmedProfile = useCallback((account) => {
    setCLI((current) => {
      if (!current) return current
      const profiles = current.profiles.filter((profile) => profile.name !== account.name)
      return { ...current, profiles: [...profiles, account] }
    })
    setSelected(account.name)
  }, [])

  useEffect(() => { void refreshEnvironment() }, [refreshEnvironment])
  // 环境通过后再读取账号，避免缺少 CLI 时触发登录命令。
  useEffect(() => {
    if (environment?.ready) void refresh()
  }, [environment?.ready, refresh])
  useEffect(() => {
    const update = () => setPage(readPage())
    window.addEventListener('hashchange', update)
    return () => window.removeEventListener('hashchange', update)
  }, [])

  const enter = useCallback((name) => {
    sessionStorage.setItem('miaoda-profile', name)
    setEntered(name)
    location.hash = '#package'
  }, [])

  function switchAccount() {
    sessionStorage.removeItem('miaoda-profile')
    setSelected(entered)
    setEntered('')
    location.hash = '#account'
  }

  const finishInstallation = useCallback(async (success) => {
    const result = await refreshEnvironment()
    // 安装完成后自动复检，通过即进入账号连接。
    if (success && result?.ready) location.hash = '#account'
  }, [refreshEnvironment])

  const account = cli?.profiles.find(({ name }) => name === entered)
  const authenticated = environment?.ready && profileReady(account)
  const showEnvironment = page === 'environment' || !environment?.ready || environment?.installation?.status === 'running'
  const showWorkspace = authenticated && !showEnvironment && ['package', 'deploy'].includes(page)
  const title = showEnvironment ? '初始环境' : showWorkspace ? pages.find(({ id }) => id === page).label : '账号连接'

  return (
    <div className="shell">
      <aside className="sidebar">
        <a className="brand" href="#package">
          <span className="brand-mark" aria-hidden="true"><i /><i /><i /></span>
          <span>妙搭部署平台</span>
        </a>
        <div className="nav-label">工作台</div>
        <nav aria-label="工作台">
          <a href="#environment" aria-current={showEnvironment ? 'page' : undefined}>
            <span className="nav-icon" aria-hidden="true">⚙</span>初始环境
          </a>
          {pages.map(({ id, label, icon }) => authenticated ? (
            <a key={id} href={'#' + id} aria-current={page === id ? 'page' : undefined}>
              <span className="nav-icon" aria-hidden="true">{icon}</span>{label}
            </a>
          ) : (
            <span className="nav-disabled" key={id}>
              <span className="nav-icon" aria-hidden="true">{icon}</span>{label}
            </span>
          ))}
        </nav>
        <div className="sidebar-foot"><span className="local-dot" />本地工作区</div>
      </aside>
      <div className="surface">
        <header className="topbar">
          <span>工作台 <span className="separator">/</span> {title}</span>
          {authenticated && (
            <button className="account-button" onClick={switchAccount}>
              <span className="avatar" aria-hidden="true">{account.user.slice(0, 1)}</span>
              <span className="account-name">{account.name} · {account.user}</span>
              <span className="switch-label">切换账号</span>
            </button>
          )}
        </header>
        <main>
          {showEnvironment ? (
            <EnvironmentSetup report={environment} checking={environmentChecking} error={environmentError}
              onCheck={refreshEnvironment} onComplete={finishInstallation} onContinue={() => { location.hash = '#account' }} />
          ) : showWorkspace ? (
            <Workspace key={entered + ':' + page} profile={account} page={page} onAccount={switchAccount} />
          ) : (
            <AccountSetup cli={cli} selected={selected} setSelected={setSelected} checking={checking}
              error={error} refresh={refresh} onConfirmAccount={applyConfirmedProfile} onEnter={enter} />
          )}
        </main>
      </div>
    </div>
  )
}

function AccountSetup({ cli, selected, setSelected, checking, error, refresh, onConfirmAccount, onEnter }) {
  const [auth, setAuth] = useState(null)
  const [authorizing, setAuthorizing] = useState(false)
  const [refreshingQR, setRefreshingQR] = useState(false)
  const [authError, setAuthError] = useState('')
  const [tenantBusy, setTenantBusy] = useState(false)
  const profile = cli?.profiles.find(({ name }) => name === selected)
  const { id: authID, status: authStatus, profile: authProfile } = auth || {}

  useEffect(() => {
    // 刷新时暂停旧任务轮询，防止旧响应盖掉新二维码。
    if (refreshingQR || !['pending', 'done'].includes(authStatus)) return
    return listenForTask(async (signal) => {
      try {
        const result = await api('auth/' + authID, {
          params: { profile: authProfile }, signal,
        })
        if (signal.aborted) return
        if (result.status === 'pending') return 2000
        if (result.status !== 'done') {
          setAuth({ ...result, profile: authProfile })
          setAuthError(result.message || '飞书授权未完成，请刷新二维码重试。')
          return
        }
        // 完成状态一到即清除二维码，使用服务刚核实的账号进入工作台。
        if (profileReady(result.account) && result.account.name === authProfile) {
          setAuth(null)
          onConfirmAccount(result.account)
          onEnter(authProfile)
          return
        }
        const next = await refresh()
        if (signal.aborted) return
        setAuth(null)
        if (profileReady(next?.profiles.find(({ name }) => name === authProfile))) {
          onEnter(authProfile)
        } else {
          setAuthError('授权完成后未检测到有效账号，请刷新账号状态。')
        }
      } catch (err) {
        if (!signal.aborted) {
          setAuthError(err.message)
          if (err.status === 404) {
            setAuth((current) => current?.id === authID ? { ...current, status: 'expired', url: '', qr: '' } : current)
          } else {
            return 3000
          }
        }
      }
    })
  }, [authID, authStatus, authProfile, refreshingQR, refresh, onConfirmAccount, onEnter])

  async function beginAuthorization() {
    setAuthError('')
    setAuthorizing(true)
    try {
      setAuth({ ...await api('auth', { data: { profile: selected } }), profile: selected })
    } catch (err) {
      setAuthError(err.message)
    } finally {
      setAuthorizing(false)
    }
  }

  async function refreshAuthorizationQR() {
    setAuthError('')
    setRefreshingQR(true)
    try {
      const data = { profile: auth.profile }
      let result
      try {
        result = await api('auth/' + auth.id + '/refresh', { data })
      } catch (err) {
        // 本地服务重启后旧任务不存在，重新发起登录即可。
        if (err.status !== 404) throw err
        result = await api('auth', { data })
      }
      setAuth({ ...result, profile: auth.profile })
    } catch (err) {
      setAuth((current) => ({ ...current, status: 'expired', url: '', qr: '' }))
      setAuthError(err.message)
    } finally {
      setRefreshingQR(false)
    }
  }

  // 读取失败不能视为首次使用，避免误创建配置。
  const requiresSetup = cli?.installed && !cli.message && !cli.profiles.length
  return (
    <section className="setup workspace">
      <div className="page-heading">
        <h1>连接飞书账号</h1>
        <p>选择租户账号，访问该账号可见的妙搭应用。已登录账号可直接进入。</p>
      </div>
      <div className="setup-grid">
        <div className="setup-panel">
          <div className="section-heading">
            <h2>lark-cli 环境</h2>
            <span className={'pill ' + (cli?.installed ? 'success' : 'pending')}>
              {checking ? '正在检测' : cli?.installed ? '已安装' : cli ? '未安装' : '待检测'}
            </span>
          </div>
          {(error || cli?.message) && <p role="alert" className="notice danger">{error || cli.message}</p>}
          {cli && !cli.installed && (
            <>
              <p className="muted">请在初始环境页面安装飞书 CLI。</p>
              <a href="#environment">打开初始环境</a>
            </>
          )}
          {cli?.installed && !cli.message && <p className="muted">已发现 {cli.profiles.length} 个租户配置。</p>}
          {(error || cli?.message || (cli && !cli.installed)) && (
            <button className="button secondary" onClick={refresh} disabled={checking}>重新检测</button>
          )}
        </div>
        <div className="setup-panel">
          <TenantSetup installed={cli?.installed} firstRun={requiresSetup}
            disabled={checking || !!error || !!cli?.message || authorizing || !!auth}
            refresh={refresh} setSelected={setSelected} onConfirmAccount={onConfirmAccount} onBusyChange={setTenantBusy} />
          {!!cli?.profiles.length && (
            <div className="account-list" role="radiogroup" aria-label="租户账号">
              {cli.profiles.map((item) => (
                <label className="account-row" key={item.name}>
                  <input type="radio" name="profile" value={item.name} checked={selected === item.name}
                    disabled={authorizing || refreshingQR || tenantBusy} onChange={() => { setSelected(item.name); setAuth(null); setAuthError('') }} />
                  <span className="avatar" aria-hidden="true">{(item.user || item.name).slice(0, 1)}</span>
                  <span className="account-info"><strong>{item.name}</strong><small>{item.user || (item.brand === 'lark' ? 'Lark' : '飞书')}</small></span>
                  <span className={'pill ' + (profileReady(item) ? 'success' : 'pending')}>
                    {profileReady(item) ? '已登录' : item.error ? '凭据异常' : '待登录'}
                  </span>
                </label>
              ))}
            </div>
          )}
          {authError && <p className="notice danger" role="alert">{authError}</p>}
          {auth && (
            <div className="auth-details" role="status">
              {!refreshingQR && auth.url && <>
                <a href={auth.url} target="_blank" rel="noreferrer">打开飞书授权页面</a>
                {auth.qr && <img src={auth.qr} width="144" height="144" alt="飞书授权二维码" />}
                <span className="muted">请使用所选租户账号完成授权。</span>
              </>}
              {auth.status !== 'done' && (
                <button className="button secondary" onClick={refreshAuthorizationQR} disabled={refreshingQR}>
                  {refreshingQR ? '正在刷新…' : '刷新二维码'}
                </button>
              )}
            </div>
          )}
          {profile?.error?.subtype === 'storage' && (
            <p className="notice danger">无法读取钥匙串凭据。请在解锁后的系统终端启动平台，再重新检测。</p>
          )}
          {profile && <div className="button-row">
            {profileReady(profile) ? (
              <button className="button primary" disabled={checking || tenantBusy} onClick={() => onEnter(selected)}>进入工作台</button>
            ) : !auth && (
              <button className="button primary" disabled={checking || authorizing || tenantBusy || profile.error?.subtype === 'storage'}
                onClick={beginAuthorization}>{authorizing ? '正在发起…' : '登录飞书'}</button>
            )}
            {profileReady(profile) && !auth && <button className="button secondary" disabled={authorizing || tenantBusy}
              onClick={beginAuthorization}>{authorizing ? '正在发起…' : '重新授权'}</button>}
          </div>}
        </div>
      </div>
    </section>
  )
}
