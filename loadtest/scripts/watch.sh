#!/usr/bin/env bash
# ============================================================
# 压测过程中的连续采样
#
# 为什么不能只看 BEFORE/AFTER：BEFORE/AFTER 只能看到"跑完稳定后"
# 的状态 —— 连接池早就缩回 MaxIdle，峰值全被抹掉了。
# 要证明"连接池被打满"必须看到压测期间的峰值。
#
# 用法: bash watch.sh <输出文件> <间隔秒> &
#       收到 SIGTERM 时退出
# ============================================================
set -uo pipefail
OUT="${1:-/tmp/watch.txt}"
INTERVAL="${2:-2}"

: > "$OUT"
trap 'exit 0' TERM INT
while true; do
  bash "$(dirname "$0")/sample.sh" "$(date +%H:%M:%S)" >> "$OUT" 2>/dev/null
  sleep "$INTERVAL"
done
