#!/usr/bin/env bash
# ============================================================
# FEED_FORUM 压测数据准备
#
# 目的：把开发库补成"有真实负载形状"的数据集，否则压测会全部
#       落在空结果/冷路径上，测出来的数字没有参考价值。
#
# 幂等：全部用 INSERT IGNORE / 覆盖写，可重复执行。
# ============================================================
set -euo pipefail

# --default-character-set=utf8mb4：Windows 上 mysql 客户端默认走 gbk，
# 夹具里的中文会撞 "Illegal mix of collations"，必须显式指定连接字符集
MYSQL="mysql --default-character-set=utf8mb4 -uroot -pljf356368"
DB="exchangeapp"
REDIS="/c/Program Files/Redis/redis-cli.exe"

# 测试用户范围：username LIKE 'testuser\_%'
# 时间线预热用户数 / 活跃用户比例
WARM_USERS=30        # 前 30 个 testuser 预热 Redis 时间线（走推路径）
TIMELINE_ITEMS=60    # 每人时间线塞 60 篇文章
ACTIVE_NUM=7         # id % 10 < 7 → 约 70% 用户是"活跃"的

echo "=========== 1. 关注关系（Feed 拉取的基础）==========="
$MYSQL $DB 2>/dev/null <<'SQL'
INSERT IGNORE INTO follows (follower_id, following_id, is_mutual, created_at, updated_at)
SELECT u.id, v.id, 0, NOW(3), NOW(3)
FROM users u JOIN users v
WHERE u.username LIKE 'testuser\_%' AND v.username LIKE 'testuser\_%'
  AND u.id <> v.id
  AND ((u.id * 7 + v.id * 11) % 3) <> 0;
SQL
$MYSQL $DB -N -e "SELECT CONCAT('  关注关系总数: ', COUNT(*)) FROM follows;" 2>/dev/null
$MYSQL $DB -N -e "SELECT CONCAT('  平均每人关注: ', ROUND(COUNT(*)/COUNT(DISTINCT follower_id),1)) FROM follows WHERE follower_id IN (SELECT id FROM users WHERE username LIKE 'testuser\_%');" 2>/dev/null

echo "=========== 2. 点赞 / 评论 / 通知（让列表接口有真实工作量）==========="
$MYSQL $DB 2>/dev/null <<'SQL'
-- 点赞：约 1/37 的组合
INSERT IGNORE INTO likes (user_id, target_type, target_id, created_at, updated_at)
SELECT u.id, 'article', a.id, NOW(3), NOW(3)
FROM users u JOIN articles a
WHERE u.username LIKE 'testuser\_%' AND a.status = 'published' AND a.deleted_at IS NULL
  AND ((u.id * 31 + a.id * 17) % 37) = 0;

-- 评论：每 3 篇已发布文章一条（先清上次夹具数据，保证幂等）
DELETE FROM comments WHERE content LIKE '压测夹具评论 %';
INSERT INTO comments (content, user_id, article_id, like_count, created_at, updated_at)
SELECT CONCAT('压测夹具评论 ', a.id), 13 + (a.id % 50), a.id, 0, NOW(3), NOW(3)
FROM articles a
WHERE a.status = 'published' AND a.deleted_at IS NULL AND a.id % 3 = 0;

-- 通知：每 7 篇一条
DELETE FROM notifications WHERE content = '赞了你的文章';
INSERT INTO notifications (user_id, from_user_id, type, target_id, content, is_read, created_at, updated_at)
SELECT 13 + (a.id % 50), a.author_id, 'like', a.id, '赞了你的文章', 0, NOW(3), NOW(3)
FROM articles a
WHERE a.status = 'published' AND a.deleted_at IS NULL AND a.id % 7 = 0;

-- 回填计数字段（热榜打分依赖这两列）
UPDATE articles a SET like_count =
  (SELECT COUNT(*) FROM likes l WHERE l.target_type = 'article' AND l.target_id = a.id);
UPDATE articles a SET comment_count =
  (SELECT COUNT(*) FROM comments c WHERE c.article_id = a.id);
SQL
$MYSQL $DB -N -e "SELECT CONCAT('  点赞: ', (SELECT COUNT(*) FROM likes), ' | 评论: ', (SELECT COUNT(*) FROM comments), ' | 通知: ', (SELECT COUNT(*) FROM notifications));" 2>/dev/null

echo "=========== 3. Redis 活跃标记（活跃度判断的数据源）==========="
# 直接用 redis-cli 批量写，构造"约 70% 活跃 / 30% 不活跃"的真实分布
# tr -d '\r'：Windows 上 mysql 客户端输出 CRLF，不剥掉会让 $(( )) 算术炸掉
{
  $MYSQL $DB -N -e "SELECT id FROM users WHERE username LIKE 'testuser\_%' ORDER BY id;" 2>/dev/null
} | tr -d '\r' > /tmp/test_uids.txt

# 先清掉历史遗留的活跃/节流 key。活跃标记是**被测代码自己会写的副作用**，
# 之前跑过的压测/登录会把所有人刷成活跃，不清就会和本次夹具的 70% 打架。
"$REDIS" --raw eval "
  for _, k in ipairs(redis.call('keys','user:active:*')) do redis.call('del', k) end
  return 1" 0 > /dev/null

TOTAL=0; ACTIVE=0
# 注意：不要把 while 循环直接管道给 redis-cli —— 管道会开子 shell，
# 循环里累加的 TOTAL/ACTIVE 出不来。先写文件，再单独喂给 redis-cli。
: > /tmp/redis_active_cmds.txt
while read -r uid; do
  [ -z "$uid" ] && continue
  TOTAL=$((TOTAL + 1))
  if [ $((uid % 10)) -lt "$ACTIVE_NUM" ]; then
    echo "SET user:active:$uid 1 EX 604800" >> /tmp/redis_active_cmds.txt
    ACTIVE=$((ACTIVE + 1))
  fi
done < /tmp/test_uids.txt
"$REDIS" < /tmp/redis_active_cmds.txt > /dev/null
echo "  testuser 总数: $TOTAL，写入活跃标记: $ACTIVE（约 $((ACTIVE * 100 / TOTAL))%）"

echo "=========== 4. 导出真实的已发布文章 ID（给 k6 用）==========="
# 不能拿 1..MAX(id) 当 ID 池：articles 表 502 行但 id 稀疏到 1002，
# 随机取会有一半落空 → 404 被空值缓存兜住，压出来的是假数据。
$MYSQL $DB -N -e "SELECT id FROM articles WHERE status='published' AND deleted_at IS NULL ORDER BY id;" 2>/dev/null \
  | tr -d '\r' | grep -v '^$' > /tmp/pub_ids.txt
{
  echo "["
  awk 'NR>1{printf ","} {printf "\n  %s", $0} END{printf "\n]\n"}' /tmp/pub_ids.txt
} > "$(dirname "$0")/../data/articles.json"
echo "  导出已发布文章 ID: $(wc -l < /tmp/pub_ids.txt) 个"

echo "=========== 5. Redis 时间线预热（Feed 推路径）==========="
# 取最新的 TIMELINE_ITEMS 篇已发布文章，塞进前 WARM_USERS 个 testuser 的时间线
$MYSQL $DB -N -e "SELECT id, UNIX_TIMESTAMP(created_at) FROM articles WHERE status='published' AND deleted_at IS NULL ORDER BY created_at DESC LIMIT $TIMELINE_ITEMS;" 2>/dev/null | tr -d "" > /tmp/timeline_items.txt
$MYSQL $DB -N -e "SELECT id FROM users WHERE username LIKE 'testuser\_%' ORDER BY id LIMIT $WARM_USERS;" 2>/dev/null | tr -d "" > /tmp/warm_uids.txt

: > /tmp/redis_cmds.txt
while read -r uid; do
  [ -z "$uid" ] && continue
  while read -r aid score; do
    [ -z "$aid" ] && continue
    echo "ZADD feed:timeline:$uid $score $aid" >> /tmp/redis_cmds.txt
  done < /tmp/timeline_items.txt
  echo "EXPIRE feed:timeline:$uid 2592000" >> /tmp/redis_cmds.txt
done < /tmp/warm_uids.txt
"$REDIS" < /tmp/redis_cmds.txt > /dev/null
echo "  预热时间线: $WARM_USERS 人 × $TIMELINE_ITEMS 篇"

echo
echo "=========== 数据就绪 ==========="
$MYSQL $DB -N -e "
SELECT CONCAT('  用户 ', (SELECT COUNT(*) FROM users),
              ' | 已发布文章 ', (SELECT COUNT(*) FROM articles WHERE status='published' AND deleted_at IS NULL),
              ' | 关注 ', (SELECT COUNT(*) FROM follows),
              ' | 点赞 ', (SELECT COUNT(*) FROM likes),
              ' | 评论 ', (SELECT COUNT(*) FROM comments),
              ' | 通知 ', (SELECT COUNT(*) FROM notifications));" 2>/dev/null
echo "  Redis dbsize: $("$REDIS" dbsize)"
# 这台机器是 Redis 5.0 Windows 移植版，redis-cli 不支持 --scan，用 EVAL+KEYS 数
echo "  timeline keys: $("$REDIS" --raw eval "return #redis.call('keys','feed:timeline:*')" 0)"
echo "  active keys:   $("$REDIS" --raw eval "return #redis.call('keys','user:active:[0-9]*')" 0)"
