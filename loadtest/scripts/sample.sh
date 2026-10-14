#!/usr/bin/env bash
# ============================================================
# 采样：服务端进程 / MySQL / Redis 的运行时指标
#
# 为什么需要：k6 只能看到"客户端视角"的 QPS 和延迟。
# 要解释"为什么慢"，必须同时看服务端的连接数、查询数、内存、
# 队列深度 —— 这是把"压测数字"变成"瓶颈定位"的关键一步。
#
# 用法: bash sample.sh "标签"
# ============================================================
set -uo pipefail

LABEL="${1:-snapshot}"
REDIS="/c/Program Files/Redis/redis-cli.exe"
MYSQL="mysql --default-character-set=utf8mb4 -uroot -pljf356368"

# ---- Go 进程：内存 / CPU ----
# 用 server* 通配：A/B 对比时会有 server.exe / server_v1.exe 两个二进制
proc=$(powershell.exe -NoProfile -Command \
  "Get-Process -Name server* -ErrorAction SilentlyContinue | Select-Object -First 1 | ForEach-Object { '{0},{1},{2}' -f \$_.WorkingSet64, \$_.Threads.Count, [math]::Round(\$_.CPU,1) }" \
  2>/dev/null | tr -d '\r' | grep -v '^$')
# 输出: 工作集字节, 线程数, 累计CPU秒
rss=$(echo "$proc" | cut -d, -f1)
threads=$(echo "$proc" | cut -d, -f2)
cpu=$(echo "$proc" | cut -d, -f3)

# ---- MySQL 全局状态 ----
mysql_stat=$($MYSQL -N -e "
SELECT CONCAT(
  (SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME='Threads_connected'), ',',
  (SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME='Threads_running'), ',',
  (SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME='Queries'), ',',
  (SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME='Innodb_row_lock_waits'), ',',
  (SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME='Innodb_row_lock_time_avg')
);" 2>/dev/null | tr -d '\r')

# ---- Redis 状态 ----
redis_info=$("$REDIS" --raw info 2>/dev/null | tr -d '\r')
r_clients=$(echo "$redis_info" | sed -n 's/^connected_clients:\(.*\)$/\1/p')
r_cmds=$(echo "$redis_info" | sed -n 's/^total_commands_processed:\(.*\)$/\1/p')
r_mem=$(echo "$redis_info" | sed -n 's/^used_memory_human:\(.*\)$/\1/p')
r_blocked=$(echo "$redis_info" | sed -n 's/^blocked_clients:\(.*\)$/\1/p')

# ---- 活跃度判断数据源的规模 ----
active_n=$("$REDIS" --raw eval "return #redis.call('keys','user:active:[0-9]*')" 0 2>/dev/null | tr -d '\r')
throttle_n=$("$REDIS" --raw eval "return #redis.call('keys','user:active:throttle:*')" 0 2>/dev/null | tr -d '\r')

printf '%s|rss=%s|threads=%s|cpuseq=%s|mysql(conn=%s,run=%s,q=%s,lockwait=%s,lockavg=%s)|redis(cli=%s,cmds=%s,mem=%s,blocked=%s)|active=%s|throttle=%s\n' \
  "$LABEL" "${rss:-NA}" "${threads:-NA}" "${cpu:-NA}" \
  "$(echo "$mysql_stat" | cut -d, -f1)" "$(echo "$mysql_stat" | cut -d, -f2)" \
  "$(echo "$mysql_stat" | cut -d, -f3)" "$(echo "$mysql_stat" | cut -d, -f4)" "$(echo "$mysql_stat" | cut -d, -f5)" \
  "${r_clients:-NA}" "${r_cmds:-NA}" "${r_mem:-NA}" "${r_blocked:-NA}" \
  "${active_n:-NA}" "${throttle_n:-NA}"
