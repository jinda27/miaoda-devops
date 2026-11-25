export function listenForTask(check) {
  const controller = new AbortController()
  const { signal } = controller
  let timer
  let running = false
  let finished = false

  async function poll() {
    // 聚焦、可见性变化和定时器可能同时触发，只保留一个检查。
    if (signal.aborted || running || finished) return
    clearTimeout(timer)
    running = true
    try {
      const delay = await check(signal)
      if (signal.aborted) return
      if (typeof delay === 'number') timer = setTimeout(poll, delay)
      else finished = true
    } finally {
      running = false
    }
  }

  function resume() {
    if (document.visibilityState === 'visible') void poll()
  }

  window.addEventListener('focus', resume)
  document.addEventListener('visibilitychange', resume)
  void poll()
  return () => {
    controller.abort()
    clearTimeout(timer)
    window.removeEventListener('focus', resume)
    document.removeEventListener('visibilitychange', resume)
  }
}
