#!/usr/bin/env bash
# 启动带调试端口的 Chrome,供 hub onboard 浏览器执行器接管。
#
# 为什么必须指定 --user-data-dir:
#   日常 Chrome 占用默认 profile 时,若只带 --remote-debugging-port 再启动,
#   Chrome 会静默新建一个临时 profile(落在 /var/folders 下),里面是空的、
#   且退出即回收——表现为"登录状态被清了"(实际原 profile 完好)。
#   这里固定用独立持久化 profile,登录态长期保留,且与日常浏览器完全隔离。
#
# 用法:
#   ./scripts/chrome-debug.sh                # 默认端口 9222
#   ./scripts/chrome-debug.sh 9333           # 自定义端口
#
# 首次使用:在打开的窗口里登录一次目标平台(飞书/钉钉/企微/腾讯文档),之后长期有效。
# 退出:直接关这个 Chrome 窗口即可;下次用同一命令重启,登录态仍在。

set -euo pipefail

PORT="${1:-9222}"
PROFILE_DIR="${CHROME_DEBUG_PROFILE:-$HOME/.chrome-debug-profile}"

# 按优先级探测 Chrome 可执行文件
find_chrome() {
  local candidates=(
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
    "/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary"
    "/Applications/Chromium.app/Contents/MacOS/Chromium"
    "$(command -v google-chrome 2>/dev/null || true)"
    "$(command -v chromium 2>/dev/null || true)"
  )
  for c in "${candidates[@]}"; do
    if [[ -n "$c" && -x "$c" ]]; then
      echo "$c"
      return 0
    fi
  done
  return 1
}

CHROME="$(find_chrome)" || {
  echo "✗ 未找到 Chrome/Chromium 可执行文件" >&2
  echo "  请手动指定: CHROME_BIN=/path/to/chrome $0 $PORT" >&2
  exit 1
}
# 允许用环境变量覆盖探测结果
CHROME="${CHROME_BIN:-$CHROME}"

# 端口已在监听则直接提示,避免启动第二个实例
if command -v curl >/dev/null 2>&1; then
  if curl -sf --max-time 1 "http://127.0.0.1:${PORT}/json/version" >/dev/null 2>&1; then
    echo "✓ 端口 ${PORT} 已有 Chrome 调试实例在运行,无需重复启动"
    echo "  当前实例信息:"
    curl -s "http://127.0.0.1:${PORT}/json/version" | sed 's/^/    /'
    exit 0
  fi
fi

mkdir -p "$PROFILE_DIR"

echo "启动 Chrome 调试实例"
echo "  可执行文件: $CHROME"
echo "  调试端口  : $PORT"
echo "  Profile   : $PROFILE_DIR"
echo

exec "$CHROME" \
  --remote-debugging-port="$PORT" \
  --user-data-dir="$PROFILE_DIR" \
  --profile-directory=Default \
  --no-first-run \
  --no-default-browser-check
