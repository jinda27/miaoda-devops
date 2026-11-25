export function apiURL(path, params = {}) {
  const query = new URLSearchParams(params).toString()
  return '/api/' + path + (query ? '?' + query : '')
}

export async function api(path, { params, data, signal } = {}) {
  const form = data instanceof FormData
  const options = data === undefined ? { signal } : {
    signal,
    method: 'POST',
    headers: {
      'X-Platform-Request': '1',
      ...(form ? {} : { 'Content-Type': 'application/json' }),
    },
    body: form ? data : JSON.stringify(data),
  }
  const response = await fetch(apiURL(path, params), options)
  const result = await response.json().catch(() => ({}))
  // 保留状态码，让后台重启后的配置请求可以重新发起。
  if (!response.ok) {
    const error = new Error(result.message || '请求失败，请检查本地服务后重试。')
    error.status = response.status
    throw error
  }
  return result
}
