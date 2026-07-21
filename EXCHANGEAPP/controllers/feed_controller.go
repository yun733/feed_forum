package controllers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"exchangeapp/config"
	"exchangeapp/global"
	"exchangeapp/models"
	"exchangeapp/mq"
	"exchangeapp/services"
	"exchangeapp/utils"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
)

var feedCtx = context.Background()

// 全局服务实例（main.go 中初始化）
var (
	FeedPullSvc  *services.FeedPullService
	BigVSvc      *services.BigVFanoutService
	FeedDedupSvc *utils.FeedDedup
	ActiveWin    *utils.ActiveWindow // 本地滑动窗口：活跃用户 L1 缓存
)

// InitFeedServices 初始化 Feed 相关服务（由 main.go 调用）
func InitFeedServices() {
	// 本地活跃窗口：1s 批量 flush 到 Redis
	ActiveWin = utils.NewActiveWindow(global.RedisDB, 1*time.Second)

	// 注入到 global，打破循环依赖，供 Auth 中间件调用
	global.MarkActiveFunc = ActiveWin.MarkActive

	FeedPullSvc = services.NewFeedPullService(ActiveWin)
	BigVSvc = services.NewBigVFanoutService(*config.AppConfig)
	FeedDedupSvc = utils.NewFeedDedup(global.RedisDB)
	log.Println("[Feed] Services initialized: ActiveWindow, BigVFanout, FeedPull, FeedDedup")
}

// ==================== 热榜（Wilson Score + 时间衰减）====================

// GetHotFeed 热榜：使用 Wilson Score + 时间衰减
func GetHotFeed(ctx *gin.Context) {
	page, _ := strconv.Atoi(ctx.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(ctx.DefaultQuery("size", "20"))
	category := ctx.Query("category")

	var articles []models.Article
	query := global.Db.Where("status = ?", "published")
	if category != "" {
		query = query.Where("category = ?", category)
	}

	since := time.Now().Add(-30 * 24 * time.Hour)
	query.Where("created_at > ?", since).
		Preload("Author").
		Find(&articles)

	type scored struct {
		article models.Article
		score   float64
	}
	var scoredList []scored
	for _, a := range articles {
		scoredList = append(scoredList, scored{
			article: a,
			score:   calculateHotScore(a.LikeCount, a.CommentCount, a.ViewCount, a.CreatedAt),
		})
	}

	// 分数降序（冒泡排序，数据量不大时可用）
	for i := 0; i < len(scoredList); i++ {
		for j := i + 1; j < len(scoredList); j++ {
			if scoredList[j].score > scoredList[i].score {
				scoredList[i], scoredList[j] = scoredList[j], scoredList[i]
			}
		}
	}

	offset := (page - 1) * size
	if offset >= len(scoredList) {
		ctx.JSON(http.StatusOK, gin.H{"items": []models.Article{}, "total": len(scoredList)})
		return
	}

	end := offset + size
	if end > len(scoredList) {
		end = len(scoredList)
	}

	result := make([]models.Article, 0, end-offset)
	for _, s := range scoredList[offset:end] {
		result = append(result, s.article)
	}

	ctx.JSON(http.StatusOK, gin.H{
		"items": result,
		"total": len(scoredList),
	})
}

func calculateHotScore(likes, comments, views int64, createdAt time.Time) float64 {
	n := float64(likes + comments)
	N := float64(views)
	if N < 1 {
		N = 1
	}

	z := 1.96
	p := n / N
	wilsonNumerator := p + (z*z)/(2*N) - z*((p*(1-p))/N+(z*z)/(4*N*N))
	wilsonDenominator := 1 + (z*z)/N
	wilsonScore := wilsonNumerator / wilsonDenominator
	if wilsonScore < 0 {
		wilsonScore = 0
	}

	hoursAgo := time.Since(createdAt).Hours()
	gravity := 1.8
	timeDecay := 1.0 / ((hoursAgo + 2.0) / gravity)

	engagement := float64(likes)*1.0 + float64(comments)*2.0

	return wilsonScore*1000 + engagement*timeDecay*10
}

// ==================== Feed 流（游标分页 + 推拉结合 + 三层去重）====================

// FeedCursor 游标结构
// 客户端每次请求带上上一页最后一条的 Cursor，服务端用 ZREVRANGEBYSCORE 代替 ZREVRANGE
// 避免深翻页时 O(offset+limit) 的性能问题
type FeedCursor struct {
	Score     float64 `json:"score"`     // ZSet score（时间戳）
	ArticleID uint    `json:"article_id"` // 文章ID，用于同分时的精确断点
}

// Encode 将游标编码为 base64 字符串
func (c FeedCursor) Encode() string {
	raw := fmt.Sprintf("%.0f:%d", c.Score, c.ArticleID)
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor 从 base64 字符串解码游标
func DecodeCursor(s string) (*FeedCursor, error) {
	if s == "" {
		return nil, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid cursor format")
	}
	score, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return nil, err
	}
	articleID, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return nil, err
	}
	return &FeedCursor{Score: score, ArticleID: uint(articleID)}, nil
}

// GetFeed 获取关注Feed流（推拉结合 + 游标分页）
// GET /api/feed?size=10&cursor=<base64>
//
// 分页策略：游标（Cursor）代替 offset
//   - 首次请求不带 cursor，返回最新的一页
//   - 后续请求带 cursor，用 ZREVRANGEBYSCORE 按 score < cursor 查询
//   - 避免了深翻页 offset 过大时的 O(N) 扫描
//
// 去重策略（三层）：
//  1. 布隆过滤器快速判断"是否可能看过"
//  2. Redis Set 精确确认（消除布隆假阳性）
//  3. 时间戳兜底（同文章时间戳唯一）
func GetFeed(ctx *gin.Context) {
	userID := ctx.GetUint("userID")
	size, _ := strconv.Atoi(ctx.DefaultQuery("size", "10"))
	if size <= 0 || size > 50 {
		size = 10
	}
	cursorStr := ctx.Query("cursor")

	var cursor *FeedCursor
	if cursorStr != "" {
		var err error
		cursor, err = DecodeCursor(cursorStr)
		if err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid cursor"})
			return
		}
	}

	var articles []models.Article
	readIDs := make(map[uint]bool)

	// 获取已读文章 ID（用于过滤）
	if FeedDedupSvc != nil {
		var err error
		readIDs, err = FeedDedupSvc.GetReadArticleIDs(ctx, userID)
		if err != nil {
			// 降级：获取失败不影响主流程，仅不进行已读过滤
			readIDs = make(map[uint]bool)
		}
	}

	// ============ Phase 1: Push 路径 - Redis Timeline（游标分页）============
	timelineKey := fmt.Sprintf("feed:timeline:%d", userID)

	if cursor == nil {
		// 首次请求：取最新的 size 条
		articleIDs, err := global.RedisDB.ZRevRange(feedCtx, timelineKey, 0, int64(size)-1).Result()
		if err == nil && len(articleIDs) > 0 {
			articles = append(articles, fetchArticlesByIDs(articleIDs, readIDs)...)
		}
	} else {
		// 游标分页：ZREVRANGEBYSCORE 按 score < cursor 取下一批
		// max 用 (cursor.Score - 1) 确保不重复
		maxScore := fmt.Sprintf("%d", int64(cursor.Score)-1)
		articleIDs, err := global.RedisDB.ZRevRangeByScore(feedCtx, timelineKey, &redis.ZRangeBy{
			Min:    "-inf",
			Max:    maxScore,
			Offset: 0,
			Count:  int64(size),
		}).Result()
		if err == nil && len(articleIDs) > 0 {
			articles = append(articles, fetchArticlesByIDs(articleIDs, readIDs)...)
		}
	}

		// ============ Phase 2+3: Pull + 冷数据（互斥锁防击穿）============
		// 时间线为空时，多个并发请求会同时触发昂贵的 DB 扫描。
		// 用 Redis SETNX 互斥锁串行化：只有第一个请求做 Pull，其余自旋等待结果。
		if len(articles) < size && FeedPullSvc != nil {
			need := size - len(articles)
			existingIDs := buildExcludeIDs(articles, readIDs)
			pulled := fillFeedWithMutex(ctx, userID, need, existingIDs, cursor)
			articles = append(articles, pulled...)
		}

	// 过滤零值
	filtered := make([]models.Article, 0, len(articles))
	for _, a := range articles {
		if a.ID != 0 {
			filtered = append(filtered, a)
		}
	}

	// ============ 构建响应（含游标）============
	var nextCursorStr string
	hasMore := len(filtered) >= size

	if hasMore && len(filtered) > 0 {
		last := filtered[len(filtered)-1]
		nextCursor := FeedCursor{
			Score:     float64(last.CreatedAt.Unix()),
			ArticleID: last.ID,
		}
		nextCursorStr = nextCursor.Encode()
	}

	// 异步标记已读
	if FeedDedupSvc != nil && len(filtered) > 0 {
		go func() {
			items := make([]utils.FeedReadItem, len(filtered))
			for i, a := range filtered {
				items[i] = utils.FeedReadItem{
					ArticleID: a.ID,
					Timestamp: a.CreatedAt.Unix(),
			}
			}
			_ = FeedDedupSvc.MarkBatchAsRead(context.Background(), userID, items)
		}()
	}

	ctx.JSON(http.StatusOK, gin.H{
		"items":      filtered,
		"size":       size,
		"next_cursor": nextCursorStr,
		"has_more":   hasMore,
	})
}

// ==================== Feed 辅助函数 ====================

// fetchArticlesByIDs 从 Redis ZSet 的 member（articleID 列表）批量查 MySQL
// 保持 ZSet 顺序，过滤已读
func fetchArticlesByIDs(articleIDs []string, readIDs map[uint]bool) []models.Article {
	if len(articleIDs) == 0 {
		return nil
	}

	var ids []uint
	idOrder := make(map[uint]int)
	for i, sid := range articleIDs {
		id, err := strconv.ParseUint(sid, 10, 64)
		if err != nil {
			continue
		}
		uid := uint(id)
		// 跳过已读
		if readIDs[uid] {
			continue
		}
		ids = append(ids, uid)
		idOrder[uid] = i
	}

	if len(ids) == 0 {
		return nil
	}

	var unsorted []models.Article
	global.Db.Where("id IN ? AND status = ?", ids, "published").
		Preload("Author").
		Find(&unsorted)

	articles := make([]models.Article, len(unsorted))
	for _, a := range unsorted {
		if idx, ok := idOrder[a.ID]; ok {
			articles[idx] = a
		}
	}

	// 压缩掉零值
	result := make([]models.Article, 0, len(articles))
	for _, a := range articles {
		if a.ID != 0 {
			result = append(result, a)
		}
	}
	return result
}

func containsID(ids []uint, target uint) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

// ==================== RabbitMQ Consumer ====================

// StartFeedConsumer 启动 Feed 推送消费者（由 main.go 调用）
func StartFeedConsumer() {
	log.Println("[FeedConsumer] Starting...")
	go ConsumeFeedPush()
}

// ConsumeFeedPush RabbitMQ消费方：将新文章推送到粉丝的 Redis Timeline
// 优化点：
//  1. 大V走 BigVFanoutService 分批次推送
//  2. 熔断降级：超时自动标记 pull-only
//  3. 消费失败时写入 Outbox 表兜底
func ConsumeFeedPush() {
	ch, err := global.RabbitMQConn.Channel()
	if err != nil {
		log.Printf("[FeedConsumer] Failed to create channel: %v", err)
		return
	}

	q, err := ch.QueueDeclare(mq.FeedPushQueue, true, false, false, false, nil)
	if err != nil {
		log.Printf("[FeedConsumer] Failed to declare queue: %v", err)
		return
	}
	ch.QueueBind(q.Name, mq.RoutingKeyFeedPush, mq.ExchangeName, false, nil)
	msgs, err := ch.Consume(q.Name, "", false, false, false, false, nil)
	if err != nil {
		log.Printf("[FeedConsumer] Failed to consume: %v", err)
		return
	}

	for msg := range msgs {
		var article models.Article
		if err := json.Unmarshal(msg.Body, &article); err != nil {
			log.Printf("[FeedConsumer] Failed to unmarshal: %v", err)
			msg.Ack(false)
			continue
		}

			// 只推活跃用户（7天内活跃）—— 查 Redis，不走 MySQL
			if !isUserActive(article.AuthorID) {
				msg.Ack(false)
				continue
			}

		// 使用 BigV Fanout 服务（自动区分大小V）
		if BigVSvc != nil {
			pushedCount, degraded, err := BigVSvc.FanoutArticle(article)
			if err != nil {
				log.Printf("[FeedConsumer] Fanout failed for article %d: %v (pushed=%d, degraded=%v)",
					article.ID, err, pushedCount, degraded)
			}
			if degraded {
				log.Printf("[FeedConsumer] Article %d degraded to pull-only mode", article.ID)
			}
		} else {
			// 降级：直接全量推送（无大小V区分）
			pushArticleToTimelines(article)
		}

		msg.Ack(false)
	}
}

// pushArticleToTimelines 简单全量推送（BigVSvc 不可用时的降级方案）
func pushArticleToTimelines(article models.Article) {
	var followerIDs []uint
	global.Db.Model(&models.Follow{}).Where("following_id = ?", article.AuthorID).
		Pluck("follower_id", &followerIDs)

	score := float64(article.CreatedAt.Unix())
	for _, fid := range followerIDs {
		timelineKey := fmt.Sprintf("feed:timeline:%d", fid)
		global.RedisDB.ZAdd(feedCtx, timelineKey, &redis.Z{
			Score:  score,
			Member: strconv.Itoa(int(article.ID)),
		})
		global.RedisDB.ZRemRangeByRank(feedCtx, timelineKey, 0, -501)
	}
}

// isUserActive 检查用户是否在 7 天内活跃过
// L1: ActiveWindow 本地内存（ns 级） → L2: Redis EXISTS（μs 级）
// Redis 故障时降级：返回 true（宁可多推，不可漏推）
func isUserActive(userID uint) bool {
	if ActiveWin != nil {
		return ActiveWin.IsActive(userID)
	}
	// 降级：直接查 Redis
	activeKey := fmt.Sprintf("user:active:%d", userID)
	exists, err := global.RedisDB.Exists(feedCtx, activeKey).Result()
	if err != nil {
		return true
	}
	return exists > 0
}


// ==================== Feed 时间线击穿防护 ====================

// buildExcludeIDs 合并已存在的文章 ID 和已读 ID
func buildExcludeIDs(articles []models.Article, readIDs map[uint]bool) []uint {
	ids := make([]uint, 0)
	for _, a := range articles {
		if a.ID != 0 {
			ids = append(ids, a.ID)
		}
	}
	for id := range readIDs {
		ids = append(ids, id)
	}
	return ids
}

// fillFeedWithMutex 互斥锁保护 Pull + 冷数据查询，防止缓存击穿
//
// 时间线 ZSet 为空时，多个并发请求同时触发昂贵的 DB 扫描。
// 用 Redis SETNX 锁串行化：只有第一个请求执行 Pull，其余自旋等待，
// 结果通过短期缓存（30s）共享，避免重复扫库。
func fillFeedWithMutex(ctx *gin.Context, userID uint, need int, excludeIDs []uint, cursor *FeedCursor) []models.Article {
	mutexKey := fmt.Sprintf("feed:mutex:timeline:%d", userID)
	cacheKey := fmt.Sprintf("feed:pull:cache:%d", userID)

	// 尝试获取锁
	locked, err := global.RedisDB.SetNX(feedCtx, mutexKey, "1", 3*time.Second).Result()
	if err != nil {
		// Redis 故障降级：放弃加锁，直接查（容忍瞬时击穿）
		return rebuildFeed(userID, need, excludeIDs, cursor)
	}

	if locked {
		// 抢到锁：执行重建，写短期缓存，释放锁
		result := rebuildFeed(userID, need, excludeIDs, cursor)
		data, _ := json.Marshal(result)
		global.RedisDB.Set(feedCtx, cacheKey, data, 30*time.Second)
		global.RedisDB.Del(feedCtx, mutexKey)
		return result
	}

	// 未抢到锁：自旋等待（100ms × 30 = 3s 上限）
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		cached, err := global.RedisDB.Get(feedCtx, cacheKey).Result()
		if err == nil && cached != "" {
			var result []models.Article
			if json.Unmarshal([]byte(cached), &result) == nil {
				return result
			}
		}
	}

	// 超时降级：返回空，避免拖死请求
	log.Printf("[Feed] fillFeedWithMutex timeout for user %d", userID)
	return nil
}

// rebuildFeed 执行 Pull + Cold 查询（无锁，由 fillFeedWithMutex 调用）
func rebuildFeed(userID uint, need int, excludeIDs []uint, cursor *FeedCursor) []models.Article {
	var result []models.Article

	// Phase 2: Pull 非活跃用户
	if FeedPullSvc != nil {
		pulled, err := FeedPullSvc.PullFromInactiveUsers(userID, need, excludeIDs)
		if err == nil {
			result = append(result, pulled...)
			for _, a := range pulled {
				excludeIDs = append(excludeIDs, a.ID)
			}
		}
	}

	// Phase 3: 冷数据回查
	if len(result) < need && BigVSvc != nil {
		followingIDs, _ := FeedPullSvc.GetFollowingIDs(userID)
		coldBefore := time.Now()
		if cursor != nil {
			coldBefore = time.Unix(int64(cursor.Score), 0)
		}
		cold, err := BigVSvc.GetColdArticles(userID, followingIDs, need-len(result), coldBefore)
		if err == nil {
			for _, a := range cold {
				if !containsID(excludeIDs, a.ID) {
					result = append(result, a)
					excludeIDs = append(excludeIDs, a.ID)
				}
			}
		}
	}

	return result
}
