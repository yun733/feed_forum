#!/usr/bin/env bash
# ============================================================
# 热榜瓶颈放大验证
#
# 假设：GetHotFeed 把 30 天内**全部**已发布文章拉进内存，再用
#       双层冒泡排序（O(n²)）打分排序。那么它的耗时应该随文章数
#       平方增长 —— 当前 479 篇时 n²≈23 万次比较，看不出来；
#       灌到 2 万篇就是 4 亿次比较/请求。
#
# 验证方法：同一台机器、同一份代码、只改文章总数，对比 QPS。
# ============================================================
set -uo pipefail
cd "$(dirname "$0")/.."

MYSQL="mysql --default-character-set=utf8mb4 -uroot -pljf356368"
DB="exchangeapp"
K6="/c/Users/11270/k6/k6-v2.2.0-windows-amd64/k6.exe"
ADD=20000

run_hot () {  # $1=vus  $2=tag
  bash scripts/reset_state.sh warm > /dev/null
  bash scripts/sample.sh "hot:$2@vus$1:BEFORE" >> results/serverside_hotscale.txt
  bash scripts/watch.sh "results/watch_hot_$2_vus$1.txt" 2 & local wp=$!
  local line
  line=$(cd k6 && "$K6" run -e TARGET=hot -e VUS="$1" -e DURATION=30s \
        -e OUT="../results/hot_$2_vus$1.json" ramp.js 2>&1 | grep -E '^reqs=' | tail -1)
  kill $wp 2>/dev/null; wait $wp 2>/dev/null
  echo "  hot:$2@vus$1  $line"
  printf '%-26s %s\n' "hot:$2@vus$1" "$line" >> results/summary_hotscale.txt
}

before=$($MYSQL $DB -N -e "SELECT COUNT(*) FROM articles WHERE status='published';" 2>/dev/null | tr -d '\r')
echo "▶ 扩容前：已发布文章 $before 篇"

run_hot 20  small
run_hot 100 small

echo "▶ 灌入 $ADD 篇已发布文章（created_at 都在 30 天内，确保进热榜扫描范围）"
$MYSQL $DB 2>/dev/null <<SQL
INSERT INTO articles (title, content, preview, author_id, status, view_count, category, like_count, comment_count, created_at, updated_at)
SELECT CONCAT('压测扩容文章 ', n),
       '压测扩容正文内容，用于放大热榜的全量扫描与内存排序开销。',
       '压测扩容摘要',
       13 + (n % 50), 'published', 0, 'tech', 0, 0,
       NOW(3) - INTERVAL n MINUTE, NOW(3)
-- 5 张 0..9 的数字表做笛卡尔积 = 10^5 个不同 n，再用 n < ADD 截取
FROM (
  SELECT a.d + b.d*10 + c.d*100 + d.d*1000 + e.d*10000 AS n
  FROM (SELECT 0 d UNION SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4
        UNION SELECT 5 UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9) a,
       (SELECT 0 d UNION SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4
        UNION SELECT 5 UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9) b,
       (SELECT 0 d UNION SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4
        UNION SELECT 5 UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9) c,
       (SELECT 0 d UNION SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4
        UNION SELECT 5 UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9) d,
       (SELECT 0 d UNION SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4
        UNION SELECT 5 UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9) e
) t
WHERE n < $ADD;
SQL
after=$($MYSQL $DB -N -e "SELECT COUNT(*) FROM articles WHERE status='published';" 2>/dev/null | tr -d '\r')
echo "▶ 扩容后：已发布文章 $after 篇"

sleep 5
run_hot 20  big
run_hot 100 big

echo "▶ 清理扩容数据"
$MYSQL $DB -e "DELETE FROM articles WHERE title LIKE '压测扩容文章 %';" 2>/dev/null
echo "  剩余已发布: $($MYSQL $DB -N -e "SELECT COUNT(*) FROM articles WHERE status='published';" 2>/dev/null | tr -d '\r') 篇"
