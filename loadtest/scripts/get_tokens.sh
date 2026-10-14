#!/usr/bin/env bash
# ============================================================
# 生成所有 testuser 的 JWT，输出 loadtest/data/tokens.json
#
# 为什么每个用户都要一个 token：
#   Auth 中间件的活跃度写入被 `user:active:throttle:<id>` 做了 5 分钟节流。
#   如果所有 VU 共用一个 token，活跃度判断的写路径一辈子只会被触发一次，
#   压测就完全测不到它。用多用户 token 才能让写路径暴露出来。
# ============================================================
set -euo pipefail

BASE="${BASE_URL:-http://localhost:9090}"
OUT="$(cd "$(dirname "$0")/.." && pwd)/data/tokens.json"
PASSWORD="123456"

# Windows 上 mysql 客户端输出 CRLF，必须剥掉 —— 否则 CR 会被拼进
# JSON 字符串里，服务端 ShouldBindJSON 直接 400
mapfile -t NAMES < <(
  mysql --default-character-set=utf8mb4 -uroot -pljf356368 exchangeapp -N -e \
    "SELECT username FROM users WHERE username LIKE 'testuser\\_%' ORDER BY id;" 2>/dev/null \
  | tr -d '\r' | grep -v '^$'
)

echo "  待处理用户: ${#NAMES[@]}"

json="["
first=1
ok=0
for uname in "${NAMES[@]}"; do
  resp=$(curl -s -X POST "$BASE/api/auth/login" \
    -H "Content-Type: application/json" \
    --data-binary "{\"username\":\"${uname}\",\"password\":\"${PASSWORD}\"}")
  token=$(printf '%s' "$resp" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
  uid=$(printf '%s' "$resp" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  if [ -z "$token" ] || [ -z "$uid" ]; then
    echo "  ⚠️  跳过 $uname（登录失败: $(printf '%s' "$resp" | head -c 80)）" >&2
    continue
  fi
  [ $first -eq 0 ] && json+=","
  first=0
  json+=$(printf '\n  {"token":"%s","userId":%s,"username":"%s"}' "$token" "$uid" "$uname")
  ok=$((ok + 1))
done
json+=$'\n]\n'

printf '%s' "$json" > "$OUT"
echo "✅ tokens.json 生成完成：$ok 个用户 → $OUT"
