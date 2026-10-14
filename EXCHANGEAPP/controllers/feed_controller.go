package controllers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"exchangeapp/config"
	"exchangeapp/global"
	"exchangeapp/models"
	"exchangeapp/mq"
	"exchangeapp/services"
	"exchangeapp/utils"
	"fmt"
	"log"
	"net/http"
	"sort"
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
	ActiveWin    *utils.ActiveWindow           // 本地滑动窗口：活跃用户 L1 缓存
	OutboxSvc    *services.AuthorOutboxService // C 类作者 outbox（全局共享）
)

// InitFeedServices 初始化 Feed 相关服务（由 main.go 调用）
func InitFeedServices() {
	// 本地活跃窗口：1s 批量 flush 到 Redis
	ActiveWin = utils.NewActiveWindow(global.RedisDB, 1*time.Second)

	// 注入到 global，打破循环依赖，供 Auth 中间件调用。
	// 用 MarkActiveAndDetectReturn 而不是裸 MarkActive：返回值告诉读路径
	// 「这个用户刚从 7 天窗口外回来」，他的 timeline 需要强制重建（维度二）
	global.MarkActiveFunc = ActiveWin.MarkActiveAndDetectReturn

	FeedPullSvc = services.NewFeedPullService(ActiveWin)
	OutboxSvc = services.NewAuthorOutboxService(*config.AppConfig)
	BigVSvc = services.NewBigVFanoutService(*config.AppConfig, ActiveWin, OutboxSvc)
	BigVSvc.Start()
	FeedDedupSvc = utils.NewFeedDedup(global.RedisDB)

	budget := config.AppConfig.Feed.InboxMemoryBudgetMB
	perUser := config.AppConfig.Feed.TimelineBytesPerUser
	log.Printf("[Feed] Services initialized: ActiveWindow, AuthorOutbox, BigVFanout, FeedPull, FeedDedup")
	log.Printf("[Feed] Threshold derivation: %dMB / %dB per inbox = %d materializable inboxes",
		budget, perUser, services.InboxesWithinBudget(budget, perUser))
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
	Score     float64 `json:"score"`      // ZSet score（时间戳）
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

	// ============ Phase 1: 一次 pipeline 读三样东西 ============
	//
	//	① feed:read:<uid>          SMEMBERS          —— 已读集合（用于过滤）
	//	② feed:timeline:<uid>      ZREVRANGEBYSCORE  —— 我自己的 timeline（A/B 类作者的推送）
	//	③ author:outbox:<cid> × K  ZREVRANGEBYSCORE  —— 我关注的 K 个大V的 outbox
	//
	// 这三者互相不依赖（③ 只需要「我关注了哪些大V」，那个列表走本地缓存），
	// 所以全部塞进**一次** pipeline。压测显示 feed 接口的瓶颈是往返次数
	// （每请求 4 次串行 Redis 往返把 QPS 钉在 ~3900），不是命令数 ——
	// 所以多读 K 个 outbox 的增量往返是 **0**，代价只是多 K 条命令。
	//
	// 为什么 outbox 归并必须在主路径上、而不是 Phase 2/3 里：
	// Phase 2/3 只在「时间线装不满一页」时才跑。而一个大V发文章后，
	// 读者的 timeline 通常是满的（关注了别的小作者），归并就永远不会发生 ——
	// 大V的新文章会永久不可见。所以必须合并进主路径。
	candidates, readIDs := collectFeedCandidates(userID, size, cursor)
	articles := fetchArticlesByIDs(candidates, readIDs)

	// ============ Phase 2+3: Pull + 冷数据（互斥锁防击穿）============
	//
	// 时间线为空时，多个并发请求会同时触发昂贵的 DB 扫描。
	// 用 Redis SETNX 互斥锁串行化：只有第一个请求做 Pull，其余自旋等待结果。
	//
	// 「用户刚回来」时强制重建：
	// 维度二（只为活跃读者物化 inbox）意味着非活跃读者的 timeline 在离开期间
	// **不更新**。只按「装不满一页」决定要不要重建的话，一个离开一周的读者
	// 会看到一周前的旧内容，而且永远等不到新内容 —— 因为他的 timeline 是满的。
	// 所以「刚回来」必须强制走一次重建，让这期间漏掉的推文能被补上。
	//
	// 判定在 Auth 中间件完成（MarkActiveAndDetectReturn）：写入活跃标记之前
	// user:active:<uid> 不存在 ⟺ 上一个 7 天窗口里他没出现过 ⟺ 期间被跳过。
	// 精确且免费（有 5 分钟节流兜着），不需要靠「头部时间戳有多旧」去猜。
	//
	// 这一次重建是**自愈**：结果会被 MaterializeInbox 写回 timeline，
	// 所以后续请求在 Phase 1 就能直接读到补上的内容，不会再触发重建。
	need := size - len(articles)
	if returning, _ := ctx.Get("feedReturning"); returning == true {
		need = size
	}
	if need > 0 && FeedPullSvc != nil {
		existingIDs := buildExcludeIDs(articles, readIDs)
		pulled := fillFeedWithMutex(ctx, userID, need, existingIDs, cursor)
		articles = append(articles, pulled...)
	}

	// 归并后重新排序 + 去重，再截断到一页
	articles = mergeAndTrim(articles, readIDs, size)

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
		"items":       filtered,
		"size":        size,
		"next_cursor": nextCursorStr,
		"has_more":    hasMore,
	})
}

// ==================== Feed 辅助函数 ====================

// 一页要 size 条，但候选池多取几倍：已读过滤会吃掉一部分，
// 只取 size 条的话过滤后可能凑不满一页，又要多跑一趟昂贵的重建。
//
// 放宽候选池的代价只是 Redis 多返回几个 member —— 排序、去重、已读过滤
// 都在 Redis 侧的内存里做完，进 MySQL 的仍然只有一页（见 collectFeedCandidates）。
const feedCandidateFactor = 3

// feedCandidateLimit 计算候选池大小（带上限，避免深翻页时放大 Redis 返回量）
func feedCandidateLimit(size int) int64 {
	n := int64(size * feedCandidateFactor)
	if n < 30 {
		n = 30
	}
	if n > 100 {
		n = 100
	}
	return n
}

// 一段 Redis ZSet 结果 → 候选列表
func appendScored(zs []redis.Z, out []feedCandidate) []feedCandidate {
	for _, z := range zs {
		id, err := strconv.ParseUint(fmt.Sprint(z.Member), 10, 64)
		if err != nil || id == 0 {
			continue
		}
		out = append(out, feedCandidate{id: uint(id), score: int64(z.Score)})
	}
	return out
}

type feedCandidate struct {
	id    uint
	score int64
}

// collectFeedCandidates 一次 pipeline 取回「已读集合 + 我的 timeline + 我关注的大V outbox」
// 归并成按时间倒序的候选文章 ID 列表，同时返回已读集合
//
// 注意这里只发**一次** Redis 往返。这些命令互相不依赖：
// 读大V outbox 需要「我关注了哪些大V」，但那个列表走本地缓存（0 网络 IO），
// 所以可以和第 ① ② 步一起打包发出去。
func collectFeedCandidates(userID uint, size int, cursor *FeedCursor) ([]uint, map[uint]bool) {
	timelineKey := fmt.Sprintf("feed:timeline:%d", userID)
	readKey := fmt.Sprintf("feed:read:%d", userID)
	limit := feedCandidateLimit(size)

	var beforeScore int64
	if cursor != nil {
		beforeScore = int64(cursor.Score)
	}

	// 「我关注的大V」：两次本地缓存查找（TierC 集合 + 关注列表），0 网络 IO。
	// 系统里没有 C 类作者时直接短路，新增逻辑的开销严格为 0。
	var tierCAuthors []uint
	if OutboxSvc != nil && OutboxSvc.HasTierCAuthors() && FeedPullSvc != nil {
		if followingIDs, err := FeedPullSvc.GetFollowingIDs(userID); err == nil {
			tierCAuthors = OutboxSvc.FollowedTierCAuthors(followingIDs)
		}
	}

	// 先把命中本地缓存的那部分大V挑出来，只把未命中的挂进 pipeline
	var cachedHeads map[uint][]services.OutboxEntry
	var missAuthors []uint
	if len(tierCAuthors) > 0 {
		cachedHeads, missAuthors = OutboxSvc.HeadBatch(tierCAuthors, cursor == nil)
	}

	pipe := global.RedisDB.Pipeline()
	readCmd := pipe.SMembers(feedCtx, readKey)

	var timelineCmd *redis.ZSliceCmd
	if beforeScore > 0 {
		// 游标分页：max 用 cursor.Score - 1，和改造前的语义保持一致
		timelineCmd = pipe.ZRevRangeByScoreWithScores(feedCtx, timelineKey, &redis.ZRangeBy{
			Min:    "-inf",
			Max:    strconv.FormatInt(beforeScore-1, 10),
			Offset: 0,
			Count:  limit,
		})
	} else {
		timelineCmd = pipe.ZRevRangeWithScores(feedCtx, timelineKey, 0, limit-1)
	}

	var outboxCmds []*redis.ZSliceCmd
	if len(missAuthors) > 0 {
		outboxCmds = OutboxSvc.QueueFetch(pipe, missAuthors, beforeScore)
	}

	// 单个 key 不存在时 Redis 返回空数组而不是错误，所以这里的错误基本都是真故障。
	// 解析阶段仍然逐个 cmd 取值：哪怕 Exec 报了错，能拿到的结果也不该丢。
	if _, err := pipe.Exec(feedCtx); err != nil {
		log.Printf("[Feed] pipeline exec failed for user %d: %v", userID, err)
	}

	// ① 已读集合
	readIDs := utils.ParseReadMembers(readCmd.Val())

	// ② timeline + ③ outbox 归并
	candidates := make([]feedCandidate, 0, limit*2)
	candidates = appendScored(timelineCmd.Val(), candidates)

	if len(missAuthors) > 0 {
		fetched := OutboxSvc.CollectFetch(missAuthors, outboxCmds, cursor == nil)
		for _, entries := range fetched {
			for _, e := range entries {
				candidates = append(candidates, feedCandidate{id: e.ArticleID, score: e.Score})
			}
		}
	}
	for _, entries := range cachedHeads {
		for _, e := range entries {
			candidates = append(candidates, feedCandidate{id: e.ArticleID, score: e.Score})
		}
	}

	if len(candidates) == 0 {
		return nil, readIDs
	}

	// 同分时用 articleID 兜底排序，保证顺序稳定（否则同一秒的两篇文章顺序可能抖动）
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].id > candidates[j].id
	})

	// 在**进 MySQL 之前**就把候选池裁到一页。
	//
	// score 就是发文时间（timeline 和 outbox 都用 CreatedAt.Unix()），
	// 所以排序、去重、按已读过滤全都能在这里做，不用等 MySQL 把行捞回来。
	// 这样候选池可以取得宽一些（吸收已读过滤，避免动不动就触发重建），
	// 而 MySQL 的负担仍然只有 size 行 —— 和改造前一样，没有为了归并而变重。
	ids := make([]uint, 0, size)
	seen := make(map[uint]bool, len(candidates))
	for _, c := range candidates {
		if seen[c.id] || readIDs[c.id] {
			continue
		}
		seen[c.id] = true
		ids = append(ids, c.id)
		if len(ids) >= size {
			break
		}
	}
	return ids, readIDs
}

// mergeAndTrim 把主路径结果和重建结果归并到一起：
// 按时间倒序 → 去重 → 过滤已读 → 截断到一页
//
// 两个来源的排序键语义必须一致：timeline 的 score、outbox 的 score、
// 以及这里用的 CreatedAt，都是「发文时间 CreatedAt.Unix()」。
func mergeAndTrim(articles []models.Article, readIDs map[uint]bool, size int) []models.Article {
	if len(articles) == 0 {
		return articles
	}

	filtered := make([]models.Article, 0, len(articles))
	seen := make(map[uint]bool, len(articles))
	for _, a := range articles {
		if a.ID == 0 || seen[a.ID] || readIDs[a.ID] {
			continue
		}
		seen[a.ID] = true
		filtered = append(filtered, a)
	}

	sort.SliceStable(filtered, func(i, j int) bool {
		if !filtered[i].CreatedAt.Equal(filtered[j].CreatedAt) {
			return filtered[i].CreatedAt.After(filtered[j].CreatedAt)
		}
		return filtered[i].ID > filtered[j].ID
	})

	if len(filtered) > size {
		filtered = filtered[:size]
	}
	return filtered
}

// fetchArticlesByIDs 按候选顺序批量查 MySQL，保持候选顺序、过滤已读
func fetchArticlesByIDs(articleIDs []uint, readIDs map[uint]bool) []models.Article {
	if len(articleIDs) == 0 {
		return nil
	}

	var ids []uint
	idOrder := make(map[uint]int, len(articleIDs))
	for i, uid := range articleIDs {
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

// ConsumeFeedPush RabbitMQ消费方：将新文章按粉丝量路由到推/拉路径
// 优化点：
//  1. Qos 背压：消息不再无上限灌进内存（P7）
//  2. 路由在写之前按粉丝量决定（A 同步推 / B 异步推 / C 只写 outbox）
//  3. 失败进重试队列，不再写没人读的 feed:degraded 标记
func ConsumeFeedPush() {
	ch, err := global.RabbitMQConn.Channel()
	if err != nil {
		log.Printf("[FeedConsumer] Failed to create channel: %v", err)
		return
	}

	q, err := ch.QueueDeclare(mq.FeedPushQueue, true, false, false, false, mq.DLXTable(mq.FeedPushQueue))
	if err != nil {
		log.Printf("[FeedConsumer] Failed to declare queue: %v", err)
		return
	}
	ch.QueueBind(q.Name, mq.RoutingKeyFeedPush, mq.ExchangeName, false, nil)

	// 背压：改造前 prefetch 无上限，Broker 会把消息一直往消费者内存里灌。
	// 一条 fanout 要跑几秒到几十秒，这段时间能积压上万条消息。
	// 设成有界之后，Broker 不再往这个消费者发新消息，积压留在队列里
	// （可见、可告警），而不是变成进程的 RSS。
	prefetch := config.AppConfig.Feed.ConsumerPrefetch
	if prefetch <= 0 {
		prefetch = 32
	}
	if err := ch.Qos(prefetch, 0, false); err != nil {
		log.Printf("[FeedConsumer] Failed to set Qos: %v", err)
	}
	msgs, err := ch.Consume(q.Name, "", false, false, false, false, nil)
	if err != nil {
		log.Printf("[FeedConsumer] Failed to consume: %v", err)
		return
	}
	log.Printf("[FeedConsumer] Consuming with prefetch=%d", prefetch)

	for msg := range msgs {
		var article models.Article
		if err := json.Unmarshal(msg.Body, &article); err != nil {
			log.Printf("[FeedConsumer] Failed to unmarshal: %v", err)
			msg.Ack(false)
			continue
		}

		// 作者本人 7 天没活跃过 → 不推，粉丝读的时候由
		// FeedPullService.PullFromInactiveUsers 回查（推拉结合的「拉」那一半）
		if !isUserActive(article.AuthorID) {
			msg.Ack(false)
			continue
		}

		if BigVSvc != nil {
			outcome := BigVSvc.FanoutArticle(article)
			if outcome.Err != nil {
				// 失败已经进了重试队列，这里只需要记日志。
				// 路由本身是按粉丝量定的，不会因为失败就静默换策略（P1）
				log.Printf("[FeedConsumer] Fanout error: %s pushed=%d skipped_inactive=%d err=%v",
					outcome.Decision.Describe(article.ID), outcome.Pushed, outcome.SkippedInactive, outcome.Err)
			} else if outcome.Deferred {
				log.Printf("[FeedConsumer] %s queued for async fanout",
					outcome.Decision.Describe(article.ID))
			}
		} else {
			// 降级：直接全量推送（无分层）
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

// ==================== Feed 缓存击穿防护（v2）====================
//
// v1 的四个缺陷（都由压测暴露，见 压测报告_FEED_FORUM.md §4.3）：
//  1. 锁 value 恒为 "1"、释放时无条件 DEL —— A 的锁 TTL 过期后 B 抢到锁，
//     A 的 defer 会把 B 的锁删掉，锁彻底失效
//  2. 重建完成后立刻 DEL 锁 —— 下一个请求又能抢到锁重新扫库，
//     "防击穿"退化成"每次冲突多扫一次库"
//  3. 自旋等待是固定 100ms 的 time.Sleep —— P50=2.75ms / P95=100.89ms
//     之间那道断崖就是这么来的，把 QPS 天花板硬钉在 2700
//  4. 自旋超时后 return nil —— 用户看到白屏，监控上看不到任何错误
//
// v2 的执行顺序改成三层：
//
//	① 先读新鲜缓存，命中直接返回（绝大多数请求走这条，连锁都不碰）
//	② 未命中才抢 SETNX 锁；抢到的重建，其余自旋等待
//	③ 自旋超时退回陈旧缓存，而不是返回空
const (
	// feedLockTTL 锁的 TTL：抢到锁的请求在这段时间内独占重建权。
	// 重建完成后**不主动释放**，让锁自然过期（理由见 fillFeedWithMutex 内注释）。
	feedLockTTL = 3 * time.Second

	// feedCacheTTL 新鲜缓存 TTL：正常路径读它
	feedCacheTTL = 30 * time.Second

	// feedStaleTTL 陈旧缓存 TTL：新缓存还没生成、且自旋超时时兜底。
	// 故意留得比锁 TTL 长得多，保证"超时也有一份旧数据"而不是白屏。
	feedStaleTTL = 10 * time.Minute

	// 自旋轮询：第一次 5ms，之后指数退避，封顶 50ms
	feedSpinBase = 5 * time.Millisecond
	feedSpinMax  = 50 * time.Millisecond

	// 重建时多拉几倍：缓存里存的是"候选池"，不是"本次要的这几条"。
	// 因为 GetFeed 每次返回后都会把这批文章异步标记为已读，
	// 如果缓存只存 need 条，下一次请求读缓存时它们已经全在已读集合里，
	// 会被 trimCached 全部过滤掉 —— 缓存等于白做。
	// （这是 v2 第一版实测踩到的坑：第二个请求返回了空数组。）
	feedPoolFactor = 4
	feedPoolMin    = 40
	feedPoolMax    = 100
)

// feedPoolSize 计算重建时要拉取的候选池大小
func feedPoolSize(need int) int {
	n := need * feedPoolFactor
	if n < feedPoolMin {
		n = feedPoolMin
	}
	if n > feedPoolMax {
		n = feedPoolMax
	}
	return n
}

// releaseFeedLockScript 只有锁的 owner 是自己时才删除。
// 不用无脑 DEL：GET+DEL 必须原子，否则"判断通过"和"真正删除"之间
// 锁可能已经过期、被别的请求重新持有。
var releaseFeedLockScript = redis.NewScript(`
	if redis.call('get', KEYS[1]) == ARGV[1] then
		return redis.call('del', KEYS[1])
	end
	return 0
`)

// newLockToken 生成锁的 owner 标识。
// 不用第三方 uuid 依赖，crypto/rand 16 字节足够了。
func newLockToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 退化方案：进程内自增 + 纳秒时间戳，仍然能区分不同持有者
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

// readCachedArticles 读缓存。区分三态：
//   - Redis 报错 / key 不存在 → ok=false（当作没缓存）
//   - 内容损坏 → ok=false（当作没缓存，下次重建覆盖）
//   - 命中（**包括命中空数组**）→ ok=true
//
// 空数组算命中很关键：否则"这个用户确实没有可拉取的内容"会被反复
// 当成缓存未命中，每个请求都去扫一次库。
func readCachedArticles(key string) ([]models.Article, bool) {
	raw, err := global.RedisDB.Get(feedCtx, key).Result()
	if err != nil || raw == "" {
		return nil, false
	}
	var result []models.Article
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, false
	}
	return result, true
}

// writeCachedArticles 同时写新鲜缓存和陈旧缓存，返回是否写入成功。
// 两个 key 用一次 Pipeline 发出去，只花 1 次往返。
func writeCachedArticles(cacheKey, staleKey string, result []models.Article) bool {
	if result == nil {
		// 不能把 nil 序列化成 "null"：读回来 json.Unmarshal("null") 也会成功，
		// 那样就分不清"缓存了一个空结果"和"压根没缓存"。
		result = []models.Article{}
	}
	data, err := json.Marshal(result)
	if err != nil {
		return false
	}
	pipe := global.RedisDB.Pipeline()
	pipe.Set(feedCtx, cacheKey, data, feedCacheTTL)
	pipe.Set(feedCtx, staleKey, data, feedStaleTTL)
	_, err = pipe.Exec(feedCtx)
	return err == nil
}

// trimCached 用当前请求的 need / excludeIDs 裁剪缓存结果。
//
// 缓存是 30s 前为**另一个请求**生成的，它的排除集和需要的条数都可能不同。
// 直接返回会把已读文章重复推给用户 —— 所以读缓存后必须再过一遍。
// （v1 的自旋路径也有这个问题，只是命中率低所以没暴露。）
func trimCached(cached []models.Article, need int, excludeIDs []uint) []models.Article {
	if len(cached) == 0 {
		return []models.Article{}
	}
	ex := make(map[uint]bool, len(excludeIDs))
	for _, id := range excludeIDs {
		ex[id] = true
	}
	out := make([]models.Article, 0, need)
	for _, a := range cached {
		if a.ID == 0 || ex[a.ID] {
			continue
		}
		out = append(out, a)
		if len(out) >= need {
			break
		}
	}
	return out
}

// fillFeedWithMutex 互斥锁保护 Pull + 冷数据查询，防止缓存击穿
//
// 时间线装不满 size 条时，多个并发请求会同时触发昂贵的 DB 扫描。
// 用 Redis SETNX 锁串行化：只有第一个请求重建，其余等待并共享结果。
func fillFeedWithMutex(ctx *gin.Context, userID uint, need int, excludeIDs []uint, cursor *FeedCursor) []models.Article {
	mutexKey := fmt.Sprintf("feed:mutex:timeline:%d", userID)
	cacheKey := fmt.Sprintf("feed:pull:cache:%d", userID)
	staleKey := fmt.Sprintf("feed:pull:stale:%d", userID)

	// ① 缓存优先。v1 直接去抢锁，导致"锁一空就重建" —— 锁在重建后立刻
	//    被 DEL，下一个落到这里的请求又能抢到锁，重复扫库。
	//    加上这一步之后，一个缓存周期（30s）内每个用户最多重建一次。
	if cached, ok := readCachedArticles(cacheKey); ok {
		return trimCached(cached, need, excludeIDs)
	}

	// ② 抢锁（带 owner token）
	token := newLockToken()
	locked, err := global.RedisDB.SetNX(feedCtx, mutexKey, token, feedLockTTL).Result()
	if err != nil {
		// Redis 故障降级：放弃加锁，直接查（容忍瞬时击穿）
		return rebuildFeed(userID, need, excludeIDs, cursor)
	}

	if locked {
		// 按候选池大小重建并缓存，再裁剪出本次需要的 need 条返回
		pool := rebuildFeed(userID, feedPoolSize(need), excludeIDs, cursor)
		if !writeCachedArticles(cacheKey, staleKey, pool) {
			// 缓存没写成功：继续占着锁只会让其他请求白白自旋整个 TTL。
			// 用 owner token 安全释放，让下一个请求立刻重试。
			releaseFeedLock(userID, token)
		}

		// 把重建结果**真正补进 timeline**，而不是只留一份 30 秒的读结果。
		//
		// 没有这一步的话，「补内容」只对当前这个请求有效：下一个请求
		// need 又变回 size-len(articles)=0（页面是满的），fillFeedWithMutex
		// 压根不会被调用，那份 feed:pull:cache 就永远读不到了 ——
		// 用户刷新一下就退回旧内容。写进 timeline 才是真的补上了。
		//
		// 只在首屏重建时物化：带 cursor 的重建拉的是**更旧**的内容，
		// 写进去也会立刻被 ZRemRangeByRank 当最旧的裁掉，白写一趟。
		if cursor == nil && BigVSvc != nil {
			if n, mErr := BigVSvc.MaterializeInbox(userID, pool); mErr != nil {
				// 物化失败不影响本次响应（pool 本来就要返回），下次请求会再重建
				log.Printf("[Feed] materialize inbox for user %d failed: %v", userID, mErr)
			} else if n > 0 {
				log.Printf("[Feed] materialized %d articles into feed:timeline:%d", n, userID)
			}
		}

		// 写成功时**不释放**锁，让它按 feedLockTTL 自然过期。
		// 理由：立刻释放的话，重建窗口内到达的并发请求虽然能在第 ① 步
		// 命中缓存，但"缓存刚好过期 + 锁刚好被释放"这个空档里会再次扫库，
		// 锁就白加了。保留 TTL 才能让并发请求真的去等待而不是重复扫库。
		return trimCached(pool, need, excludeIDs)
	}

	// ③ 未抢到锁：自旋等待。
	//    v1 是固定 100ms（`time.Sleep(100 * time.Millisecond)`），把 P95
	//    硬生生钉在 100ms 上。改成 5ms 起步、指数退避到 50ms：
	//    既能第一时间拿到刚写好的缓存，又不会在长等待时把 Redis 打爆
	//    （固定 5ms 死循环 3 秒 = 单请求 600 次 GET）。
	deadline := time.Now().Add(feedLockTTL)
	interval := feedSpinBase
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		if cached, ok := readCachedArticles(cacheKey); ok {
			return trimCached(cached, need, excludeIDs)
		}
		if interval < feedSpinMax {
			interval *= 2
			if interval > feedSpinMax {
				interval = feedSpinMax
			}
		}
	}

	// ④ 超时降级：退回陈旧缓存（最多 10 分钟前的那一份）。
	//    v1 这里 return nil —— 用户白屏，而且日志之外没有任何信号。
	if stale, ok := readCachedArticles(staleKey); ok {
		log.Printf("[Feed] fillFeedWithMutex timeout for user %d, served stale cache", userID)
		return trimCached(stale, need, excludeIDs)
	}
	log.Printf("[Feed] fillFeedWithMutex timeout for user %d, no stale cache available", userID)
	return nil
}

// releaseFeedLock 主动释放锁，只有 owner 能删成功。
//
// 正常路径**不调用**它：重建成功时让锁自然过期（见 fillFeedWithMutex）。
// 唯一的使用场景是缓存写入失败 —— 此时占着锁没有意义，早放早让别人重试。
// 这也是 owner token 存在的理由：不加校验的 DEL 会误删别人的锁。
func releaseFeedLock(userID uint, token string) bool {
	key := fmt.Sprintf("feed:mutex:timeline:%d", userID)
	ok, err := releaseFeedLockScript.Run(feedCtx, global.RedisDB, []string{key}, token).Int()
	return err == nil && ok == 1
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
