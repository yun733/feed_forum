#!/usr/bin/env bash
# ============================================================
# 阶梯加压调度器：对每个接口按 20/50/100/200/400 并发逐档压，
# 每档单独出一份 k6 JSON，并同步采样服务端指标。
#
# 每个 (接口, 并发) 组合独立跑一轮 —— 不混流量，指标才干净，
# 才能画出"并发 → QPS / P99"的拐点曲线。
#
# 用法：
#   bash run_ramp.sh <接口名...>
#   MODE=cold bash run_ramp.sh feed          # 用冷状态跑
#   CLEAR_THROTTLE=1 bash run_ramp.sh unread # 强制走活跃度写路径
# ============================================================
set -uo pipefail

cd "$(dirname "$0")/.."   # → loadtest/
K6="/c/Users/11270/k6/k6-v2.2.0-windows-amd64/k6.exe"
REDIS="/c/Program Files/Redis/redis-cli.exe"

VUS_LIST="${VUS_LIST:-20 50 100 200 400}"
DURATION="${DURATION:-30s}"
MODE="${MODE:-warm}"
CLEAR_THROTTLE="${CLEAR_THROTTLE:-0}"
COOLDOWN="${COOLDOWN:-10}"
TAG="${TAG:-}"

mkdir -p results
SUMMARY="results/summary${TAG:+_$TAG}.txt"
SERVERSIDE="results/serverside${TAG:+_$TAG}.txt"

for target in "$@"; do
  for vus in $VUS_LIST; do
    label="${target}${TAG:+:$TAG}@vus${vus}"

    echo "───────────────────────────────────────────────"
    echo "▶ $label  (${DURATION}, mode=$MODE, clear_throttle=$CLEAR_THROTTLE)"

    bash scripts/reset_state.sh "$MODE" | tee -a "$SERVERSIDE"

    if [ "$CLEAR_THROTTLE" = "1" ]; then
      ( while true; do
          "$REDIS" --raw eval "for _,k in ipairs(redis.call('keys','user:active:throttle:*')) do redis.call('del',k) end return 1" 0 >/dev/null 2>&1
          sleep 0.2
        done ) &
      CLEAR_PID=$!
      echo "  [throttle-clear] 后台清除节流 key，pid=$CLEAR_PID"
      sleep 1
    fi

    bash scripts/sample.sh "$label:BEFORE" >> "$SERVERSIDE"

    # 压测期间连续采样（2s 一次），拿峰值而不是跑完的稳定值
    WATCH="results/watch_${target}${TAG:+_$TAG}_vus${vus}.txt"
    bash scripts/watch.sh "$WATCH" 2 &
    WATCH_PID=$!

    # 服务端日志打点：Gin 会为每个请求写一行日志，这里做增量分析，
    # 拿"真实返回给客户端的状态码分布" + "GORM 慢 SQL 条数"。
    # k6 只能看到客户端视角；状态码分布和慢 SQL 只有在服务端才看得到。
    LOG_MARK=$(wc -l < /tmp/server.log 2>/dev/null || echo 0)
    LOG_BYTES=$(stat -c %s /tmp/server.log 2>/dev/null || echo 0)

    out="../results/${target}${TAG:+_$TAG}_vus${vus}.json"
    line=$(cd k6 && "$K6" run \
      -e TARGET="$target" -e VUS="$vus" -e DURATION="$DURATION" -e OUT="$out" \
      ramp.js 2>&1 | grep -E '^reqs=' | tail -1)

    kill "$WATCH_PID" 2>/dev/null || true
    wait "$WATCH_PID" 2>/dev/null || true

    # 增量日志分析
    if [ -f /tmp/server.log ]; then
      NEW_LOG="/tmp/stage_${target}${TAG:+_$TAG}_vus${vus}.log"
      tail -n +"$((LOG_MARK + 1))" /tmp/server.log > "$NEW_LOG" 2>/dev/null || true
      codes=$(awk -F'|' '/\[GIN\]/{gsub(/ /,"",$2); print $2}' "$NEW_LOG" \
              | sort | uniq -c | sort -rn | awk '{printf "%s=%s ", $2, $1}')
      slow=$(grep -c "SLOW SQL" "$NEW_LOG" 2>/dev/null || echo 0)
      slow_art=$(grep -A1 "SLOW SQL" "$NEW_LOG" 2>/dev/null | grep -c "UPDATE \`articles\`" || true)
      slow_usr=$(grep -A1 "SLOW SQL" "$NEW_LOG" 2>/dev/null | grep -c "UPDATE \`users\`" || true)
      slow_max=$(grep -oE "\[[0-9]+\.[0-9]+ms\]" "$NEW_LOG" 2>/dev/null | tr -d '[]ms' | sort -rn | head -1)
      LOG_BYTES_END=$(stat -c %s /tmp/server.log 2>/dev/null || echo 0)
      log_mb=$(( (LOG_BYTES_END - LOG_BYTES) / 1048576 ))
      echo "  LOG|codes=[$codes]|slowSQL=$slow(articles=$slow_art,users=$slow_usr)|slowSQL_max=${slow_max:-0}ms|logGrowth=${log_mb}MB" >> "$SERVERSIDE"
      echo "  LOG|codes=[$codes]|slowSQL=$slow(articles=$slow_art,users=$slow_usr)|slowSQL_max=${slow_max:-0}ms|logGrowth=${log_mb}MB"
      rm -f "$NEW_LOG"
    fi

    bash scripts/sample.sh "$label:AFTER" >> "$SERVERSIDE"
    # 压测期间峰值（连接数/线程/RSS 的最大值）单独记一行，便于报告引用
    if [ -s "$WATCH" ]; then
      peak=$(awk -v label="$label" -F'|' '
        { for (i=1;i<=NF;i++) {
            split($i, kv, "=");
            v = kv[2]+0;
            if (kv[1]=="rss" && v>rss) rss=v;
            if ($i ~ /^mysql\(conn=/) { gsub(/[^0-9]/,"",kv[2]); if (kv[2]+0>mc) mc=kv[2]+0 }
            if ($i ~ /^redis\(cli=/)  { gsub(/[^0-9]/,"",kv[2]); if (kv[2]+0>rc) rc=kv[2]+0 }
            if (kv[1]=="threads" && v>th) th=v;
          } }
        END { printf "PEAK(%s)|rss=%.0fMB|threads=%d|mysql_conn=%d|redis_cli=%d", label, rss/1048576, th, mc, rc }
      ' label="$label" "$WATCH")
      echo "$peak" >> "$SERVERSIDE"
      echo "  $peak"
    fi

    if [ "$CLEAR_THROTTLE" = "1" ]; then
      kill "$CLEAR_PID" 2>/dev/null || true
      wait "$CLEAR_PID" 2>/dev/null || true
    fi

    echo "  $line"
    printf '%-28s %s\n' "$label" "$line" >> "$SUMMARY"

    sleep "$COOLDOWN"
  done
done

echo
echo "✅ 完成。汇总：$SUMMARY    服务端采样：$SERVERSIDE"
