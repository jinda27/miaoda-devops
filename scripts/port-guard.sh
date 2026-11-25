# Shared by the desktop bundle and terminal launcher. This file is sourced.

port_listeners() {
  local port="$1" records status
  if ! command -v lsof >/dev/null 2>&1; then
    printf '%s\n' "未找到 lsof，无法检查端口。" >&2
    return 1
  fi
  records="$(lsof -nP -iTCP:"$port" -sTCP:LISTEN -Fpn)" || {
    status=$?
    [[ "$status" -eq 1 ]] || return "$status"
  }
  printf '%s\n' "$records" | awk -v port="$port" '
    /^p[0-9]+$/ { pid = substr($0, 2) }
    $0 == "n127.0.0.1:" port || $0 == "n*:" port || $0 == "n0.0.0.0:" port ||
    $0 == "n[::]:" port || $0 == "n[::ffff:127.0.0.1]:" port {
      if (pid && !seen[pid]++) print pid
    }
  ' | sort -n
}

port_processes() {
  local pid identity
  for pid in $1; do
    identity="$(ps -ww -p "$pid" -o lstart= -o comm=)" || return 1
    [[ -n "$identity" ]] || return 1
    printf 'PID %s  %s\n' "$pid" "$identity"
  done
}

confirm_port_in_terminal() {
  local answer
  printf '%s\n' "$1" >&2
  printf '%s' '结束这些进程并继续启动？[y/N] ' >&2
  read -r answer && [[ "$answer" == [yY] || "$answer" == [yY][eE][sS] ]]
}

release_port() {
  local port="$1" confirm="${2:-}" pids original message current current_processes pid attempt
  if [[ ! "$port" =~ ^[1-9][0-9]{0,4}$ ]] || [[ "$port" -gt 65535 ]]; then
    printf '%s\n' '端口必须是 1 至 65535 的整数。' >&2
    return 1
  fi
  pids="$(port_listeners "$port")" || return 1
  [[ -n "$pids" ]] || return 0
  original="$(port_processes "$pids")" || {
    printf '%s\n' "端口 $port 的占用已变化，请重新启动。" >&2
    return 1
  }
  # 终端启动可在结束进程前要求确认。
  if [[ -n "$confirm" ]]; then
    message="$(printf '端口 %s 由以下进程占用：\n\n%s\n\n结束进程可能中断正在执行的任务。' "$port" "$original")"
    "$confirm" "$message" || return 2
  fi

  # 重新核对监听进程，避免结束后来占用端口的进程。
  current="$(port_listeners "$port")" || return 1
  [[ -n "$current" ]] || return 0
  if [[ "$current" != "$pids" ]] || ! current_processes="$(port_processes "$current")" ||
      [[ "$current_processes" != "$original" ]]; then
    printf '%s\n' "端口 $port 的占用已变化，请重新启动。" >&2
    return 1
  fi
  for pid in $pids; do
    if ! kill -TERM "$pid" 2>/dev/null && kill -0 "$pid" 2>/dev/null; then
      printf '%s\n' "无法结束占用端口 $port 的进程（PID $pid）。" >&2
      return 1
    fi
  done

  for ((attempt = 0; attempt < 25; attempt++)); do
    current="$(port_listeners "$port")" || return 1
    [[ -n "$current" ]] || return 0
    sleep 0.2
  done
  printf '%s\n' "端口 $port 仍被占用，启动已停止。" >&2
  return 1
}
