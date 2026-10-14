#!/usr/bin/env bash
# ============================================================
# 压测前的状态复位：保证每一轮跑在同一个起点上
#
# 为什么必须复位：活跃标记是**被测代码自己会写的副作用**。
# 压测过程中 Auth 中间件会把每个用户都刷成"活跃"，跑完一轮
# 之后的初始状态就和上一轮不一样了，两轮数字没法对比。
#
# 用法： bash reset_state.sh <mode>
#   warm  （默认）35 个活跃 + 清空节流 → 稳态：读路径有命中也有未命中
#   cold           清空所有活跃标记     → 病态：FilterInactive 全员未活跃，全量走 DB 拉取
#   allactive      50 个全活跃 + 清空节流 → FilterInactive 直接短路返回
#   nothrottle     只清节流             → 强制活跃度**写路径**每次请求都执行
#
# 前四个 mode 保持 Redis 时间线不动（Feed 走推路径）。
# 下面两个额外清空时间线，逼 Feed 走拉路径 —— 活跃度判断的**读路径**
# 只有拉路径才会真正执行（推路径直接拿够 10 条就返回了）：
#   nopush         35 个活跃 + 无时间线 → 读路径，活跃度有命中也有未命中
#   nopush-cold    0  个活跃 + 无时间线 → 读路径最坏情况，FilterInactive 全员未命中
# ============================================================
set -euo pipefail

MODE="${1:-warm}"
REDIS="/c/Program Files/Redis/redis-cli.exe"
MYSQL="mysql --default-character-set=utf8mb4 -uroot -pljf356368"
DB="exchangeapp"
ACTIVE_NUM=7   # id % 10 < 7 → 约 70% 活跃

# 清掉 Feed 的瞬态状态：去重集合 / 拉取缓存 / 锁 / 布隆。
# 这些全都是**被测代码自己写的**：不清的话上一轮跑完时"用户已读完全部文章"，
# 下一轮起点完全不同 —— before/after 对比就失去意义了。
# 注意不要碰 feed:timeline:*（那是夹具造的数据，由下面的逻辑管理）。
"$REDIS" --raw eval "
  local n = 0
  for _, pat in ipairs({'feed:read:*','feed:bloom:read','feed:pull:cache:*','feed:pull:stale:*','feed:mutex:*'}) do
    for _, k in ipairs(redis.call('keys', pat)) do redis.call('del', k); n = n + 1 end
  end
  return n" 0 > /dev/null

# 清空所有活跃/节流 key（KEYS 只在这台单机 Redis 上用于测试复位，生产禁用）
"$REDIS" --raw eval "
  local n = 0
  for _, k in ipairs(redis.call('keys','user:active:*')) do
    redis.call('del', k); n = n + 1
  end
  return n
" 0 > /dev/null

mapfile -t UIDS < <($MYSQL $DB -N -e \
  "SELECT id FROM users WHERE username LIKE 'testuser\\_%' ORDER BY id;" 2>/dev/null \
  | tr -d '\r' | grep -v '^$')

# ── 时间线：nopush* 模式清空（逼走拉路径），其余模式从 prepare_data
#    留下的命令文件恢复（保证 30 人 × 60 篇的推路径数据稳定存在）
needs_timeline_restore=1
case "$MODE" in
  nopush|nopush-cold)
    "$REDIS" --raw eval "
      local n=0
      for _,k in ipairs(redis.call('keys','feed:timeline:*')) do redis.call('del',k); n=n+1 end
      return n" 0 > /dev/null
    needs_timeline_restore=0
    ;;
esac
if [ "$needs_timeline_restore" = "1" ] && [ -s /tmp/redis_cmds.txt ]; then
  cur=$("$REDIS" --raw eval "return #redis.call('keys','feed:timeline:*')" 0 | tr -d '\r')
  if [ "${cur:-0}" -eq 0 ]; then
    "$REDIS" < /tmp/redis_cmds.txt > /dev/null
  fi
fi

: > /tmp/reset_cmds.txt
case "$MODE" in
  nopush|warm)
    for uid in "${UIDS[@]}"; do
      if [ $((uid % 10)) -lt "$ACTIVE_NUM" ]; then
        echo "SET user:active:$uid 1 EX 604800" >> /tmp/reset_cmds.txt
      fi
    done
    ;;
  cold|nopush-cold)
    :   # 什么都不设 → 所有人都不活跃
    ;;
  allactive)
    for uid in "${UIDS[@]}"; do
      echo "SET user:active:$uid 1 EX 604800" >> /tmp/reset_cmds.txt
    done
    ;;
  nothrottle)
    for uid in "${UIDS[@]}"; do
      if [ $((uid % 10)) -lt "$ACTIVE_NUM" ]; then
        echo "SET user:active:$uid 1 EX 604800" >> /tmp/reset_cmds.txt
      fi
    done
    ;;
  *)
    echo "未知 mode: $MODE" >&2; exit 1 ;;
esac

[ -s /tmp/reset_cmds.txt ] && "$REDIS" < /tmp/reset_cmds.txt > /dev/null

echo "  [reset:$MODE] active=$("$REDIS" --raw eval "return #redis.call('keys','user:active:[0-9]*')" 0 | tr -d '\r') throttle=$("$REDIS" --raw eval "return #redis.call('keys','user:active:throttle:*')" 0 | tr -d '\r')"
