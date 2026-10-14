package services

import (
	"context"
	"exchangeapp/config"
	"exchangeapp/global"
	"exchangeapp/models"
	"exchangeapp/utils"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/go-redis/redis/v8"
)

var fanoutCtx = context.Background()

// BigVFanoutService 按粉丝量的推拉分层（§4.2）
//
// 改造前：大V分支和普通分支是同一个 batchFanout，「大V走特殊通道」只贡献了一行日志。
// 真实行为等价于「推给所有粉丝，超 1 秒中止并降级」——决策信号是运行时耗时，
// 同一篇文章两次发布结果可能不同。
//
// 改造后：路由在**写之前**由粉丝量决定（纯函数，见 FeedRouter），
//
//	A 类（< 5,000）       同步推：写成本 ≤ 1.5 万条命令
//	B 类（5,000 ~ 50 万）  异步延迟推：交给 FanoutWorkerPool，不阻塞消费主流程
//	C 类（>= 50 万）       不推，只写作者 outbox：写侧从 O(粉丝数) 降到 O(1)
//
// 超时依然保留，但语义变了：不再是「降级触发器」，只是防单条消息永久阻塞消费者的
// 保护机制。命中即告警 —— 因为路由早就按粉丝量定完了，超时只能说明 Redis 抖动。
type BigVFanoutService struct {
	cfg        config.Config
	router     *FeedRouter
	breakers   *AuthorCircuitRegistry
	activeWin  *utils.ActiveWindow  // 维度二：只为活跃读者物化 inbox
	outbox     *AuthorOutboxService // C 类作者的 outbox
	pool       *FanoutWorkerPool    // B 类的异步执行池
	retryQueue *FanoutRetryQueue    // 失败重试
	retry      *FanoutRetryWorker
}

// FanoutOutcome 一次 fanout 的结果，替代原来含糊的 (count, degraded, err)
type FanoutOutcome struct {
	Decision        RouteDecision
	Pushed          int
	SkippedInactive int
	Deferred        bool // B 类：已投递给异步 worker，推送结果稍后才产生
	Err             error
}

// NewBigVFanoutService 创建推送服务
func NewBigVFanoutService(cfg config.Config, activeWin *utils.ActiveWindow, outbox *AuthorOutboxService) *BigVFanoutService {
	timeout := time.Duration(cfg.Feed.FanoutTimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = time.Second
	}

	svc := &BigVFanoutService{
		cfg:        cfg,
		router:     NewFeedRouter(cfg),
		breakers:   NewAuthorCircuitRegistry(timeout, 10000),
		activeWin:  activeWin,
		outbox:     outbox,
		retryQueue: NewFanoutRetryQueue(cfg.Feed.FanoutMaxRetries),
	}

	// B 类作者的异步执行池。按 authorID 分区，保证同作者保序（§5.2）
	//
	// ⚠️ handler 里调的是 executeNow 而不是 execute：
	// execute 会在 B 类分支里把文章**再投回这个池**，worker 取出来又投回去，
	// 文章在队列里无限弹跳 —— 不推、不报错、吃满 CPU，且日志里什么都看不到。
	// 「要不要异步」是入口处的一次性决策，池里的 worker 只负责执行。
	svc.pool = NewFanoutWorkerPool(4, 256, func(article models.Article) {
		if err := svc.executeNow(article); err != nil {
			svc.retryQueue.Enqueue(article.ID, 0, err.Error())
		}
	})

	svc.retry = NewFanoutRetryWorker(
		svc.retryQueue,
		time.Duration(cfg.Feed.FanoutRetryScanMs)*time.Millisecond,
		func(article models.Article) error {
			// 重试时重新跑一遍完整路由：如果期间粉丝量已经变化，
			// 走新策略是正确的（比如大V掉粉了，推就划算了）。
			// 这里不自己入队 —— 入队/计数由 worker 统一负责，
			// 否则「自增 attempt 入队」和「worker 按 member 删除」会打架。
			return svc.executeNow(article)
		},
	)
	return svc
}

// Start 启动后台组件（异步池已经在构造时启动）
func (s *BigVFanoutService) Start() {
	s.retry.Start()
}

// Stop 优雅关闭
func (s *BigVFanoutService) Stop() {
	s.retry.Stop()
	s.pool.Stop()
}

// PlanArticle 只做路由决策，不执行。供统计/接口/测试使用。
func (s *BigVFanoutService) PlanArticle(article models.Article) RouteDecision {
	return s.router.Decide(s.getFollowerCount(article.AuthorID))
}

// FanoutArticle 按粉丝量路由并执行（MQ 消费者调用）
//
// 注意：路由信号只有 followerCount。同一篇文章、同一个粉丝量，路由结果恒定。
func (s *BigVFanoutService) FanoutArticle(article models.Article) FanoutOutcome {
	decision := s.PlanArticle(article)
	outcome := s.dispatch(article, decision)

	// 同步路径（A 类）失败时在这里统一入队。
	// 异步路径的入队由 worker pool 的回调负责，重试路径的入队由 retry worker 负责 ——
	// 三条路的入队点各只有一个，避免「入队」和「按 member 删除」互相打架。
	if outcome.Err != nil && !outcome.Deferred {
		if IsFanoutYield(outcome.Err) {
			s.retryQueue.EnqueueResume(article.ID, 1)
		} else {
			s.retryQueue.Enqueue(article.ID, 0, outcome.Err.Error())
		}
	}
	return outcome
}

// executeNow 执行路由决策，**不再往下游投递**
//
// 已进入 worker 池或重试队列之后调用它：此时「异步」这件事已经发生过了，
// 剩下的只有「推」或「写 outbox」两种真实动作。
//
// B 类在这里走 executePush —— 它已经在一个 worker goroutine 里了，
// 再投一次池就是无限弹跳（见 NewBigVFanoutService 的注释）。
//
// 刻意**不在这里入队**：self-enqueue 会和 retry worker 的「按 member 删除」
// 撞车 —— worker 取出 "id:1" 后，若 fanout 内部又插一条 "id:1"，
// worker 紧接着的 Complete 就会把刚插进去的那条删掉，文章静默丢失。
func (s *BigVFanoutService) executeNow(article models.Article) error {
	decision := s.router.Decide(s.getFollowerCount(article.AuthorID))

	switch decision.Strategy {
	case StrategyPullOnly:
		return s.writeOutboxOnly(article, decision).Err
	default:
		// 同步推和异步推在「已经异步执行」的语境下是同一个动作
		return s.executePush(article, decision).Err
	}
}

// dispatch 入口处的分发：只有这里会做「要不要异步」的决策
func (s *BigVFanoutService) dispatch(article models.Article, decision RouteDecision) FanoutOutcome {
	if decision.Strategy == StrategyAsyncPush {
		// B 类：交给 worker 池，不阻塞调用方
		if s.pool != nil && s.pool.Submit(article) {
			return FanoutOutcome{Decision: decision, Deferred: true}
		}
		// 队列拥塞：交给重试队列，不丢文章（Err 让调用方入队）
		return FanoutOutcome{
			Decision: decision,
			Err:      fmt.Errorf("worker pool congested, article %d deferred", article.ID),
		}
	}

	// A 类同步推 / C 类写 outbox：都在调用方 goroutine 里做完
	switch decision.Strategy {
	case StrategyPullOnly:
		return s.writeOutboxOnly(article, decision)
	default:
		return s.executePush(article, decision)
	}
}

// writeOutboxOnly C 类：不推，只写作者自己的 outbox
//
// 写侧成本 O(1)：1 次 Pipeline 对比给 1000 万粉丝写 3000 万条命令。
//
// 切换顺序（§5.1）：**先建 outbox → 再登记作者为 C 类**。
// 反过来的话，窗口期内到达的读请求会去读一个还不存在的 outbox，
// 文章就「消失」了。顺序对了，最坏情况只是读者多归并一个空 key。
func (s *BigVFanoutService) writeOutboxOnly(article models.Article, decision RouteDecision) FanoutOutcome {
	if s.outbox == nil {
		// 降级：outbox 服务不可用就只能退回推模式，否则文章谁也看不到
		log.Printf("[BigVFanout] outbox service unavailable, falling back to push for article %d", article.ID)
		return s.executePush(article, decision)
	}

	if err := s.outbox.Append(article); err != nil {
		// 只返回错误，入队交给调用方（避免和 retry worker 的记账打架）
		return FanoutOutcome{Decision: decision, Err: fmt.Errorf("outbox append: %w", err)}
	}

	// 先有 outbox 再有登记，保证不会有「文章在两边都没有」的窗口
	if err := s.outbox.RegisterTierC(article.AuthorID); err != nil {
		log.Printf("[BigVFanout] ALERT failed to register author %d as tier-C: %v", article.AuthorID, err)
	}

	log.Printf("[BigVFanout] %s → outbox only (1 ZADD, no fanout)", decision.Describe(article.ID))
	return FanoutOutcome{Decision: decision, Pushed: 0}
}

// executePush A/B 类：把文章写进粉丝的 timeline
func (s *BigVFanoutService) executePush(article models.Article, decision RouteDecision) FanoutOutcome {
	// 作者已不在 C 类（掉粉 / 阈值调整）→ 摘掉登记，自愈
	// 历史文章仍留在 outbox 里，两边可能短暂重叠，由 FeedDedup 三层去重兜底
	if s.outbox != nil && s.outbox.IsTierC(article.AuthorID) {
		if err := s.outbox.UnregisterTierC(article.AuthorID); err != nil {
			log.Printf("[BigVFanout] failed to unregister tier-C author %d: %v", article.AuthorID, err)
		}
	}

	breaker := s.breakers.Get(article.AuthorID)
	// 熔断器按作者隔离：大V打爆自己的熔断器只影响自己，不再拖垮所有小作者（P2）
	if !breaker.Allow() {
		log.Printf("[BigVFanout] circuit OPEN for author %d, article %d queued for retry",
			article.AuthorID, article.ID)
		return FanoutOutcome{Decision: decision, Err: fmt.Errorf("author %d circuit open", article.AuthorID)}
	}

	start := time.Now()
	pushed, skipped, err := s.pushToTimelines(article)
	elapsed := time.Since(start)

	if err != nil {
		// 让路不算失败：熔断器统计的是「Redis/DB 出问题了」，
		// 而让路是正常的分片推进，记进去会让大V把自己的熔断器打开。
		if !IsFanoutYield(err) {
			breaker.RecordFailure(err.Error())
		}
		log.Printf("[BigVFanout] %s stopped after %v (pushed=%d): %v",
			decision.Describe(article.ID), elapsed, pushed, err)
		return FanoutOutcome{Decision: decision, Pushed: pushed, SkippedInactive: skipped, Err: err}
	}

	// 传真实耗时，让 circuit_breaker.go 的 latency > timeout 判据真正生效（P6）。
	// 改造前这里是 RecordSuccess(ctx, 0)，硬编码 0 导致该判据永远为 false。
	breaker.RecordSuccess(fanoutCtx, elapsed)

	log.Printf("[BigVFanout] %s done in %v: pushed=%d skipped_inactive=%d",
		decision.Describe(article.ID), elapsed, pushed, skipped)
	return FanoutOutcome{Decision: decision, Pushed: pushed, SkippedInactive: skipped}
}

// MaterializeInbox 把一个读者的重建结果**真正补进他的 timeline**
//
// 用途：非活跃读者「回来」的那次重建。维度二在他离开期间跳过了他的 inbox，
// 所以要把漏掉的内容补回去 —— 补进 timeline，而不是只缓存一份读结果。
//
// 为什么必须写 timeline（这是之前实现的一个真实缺口）：
// `feed:pull:cache` 只在 `need > 0` 时才会被读到，而那个入口长这样：
//
//	need := size - len(articles)     // 读者回来时页面是满的 → 0
//	if returning { need = size }     // 是 feedReturning 把它顶成 size 的
//	if need > 0 { fillFeedWithMutex(...) }   // ← 缓存只在这里面被读
//
// 于是「只写缓存」等于：第一个请求补了一次内容 → 第二个请求 need 又变回 0
// → 缓存根本不会被读到 → 用户刷新一下又看见旧内容。
// **把结果写进 timeline，补的内容才会在下一次读的 Phase 1 里自然出现。**
//
// score 约定和 pushToTimelines 完全一致（CreatedAt.Unix()、member=articleID、
// 同样的裁剪和 TTL），所以两条路径写进去的数据在同一个时间序里可以直接归并。
func (s *BigVFanoutService) MaterializeInbox(userID uint, articles []models.Article) (int, error) {
	if len(articles) == 0 {
		return 0, nil
	}

	maxItems := int64(s.cfg.Feed.TimelineMaxItems)
	if maxItems <= 0 {
		maxItems = 500
	}
	ttlDays := s.cfg.Feed.TimelineTTLDays
	if ttlDays <= 0 {
		ttlDays = 30
	}
	ttl := time.Duration(ttlDays) * 24 * time.Hour

	timelineKey := fmt.Sprintf("feed:timeline:%d", userID)
	pipe := global.RedisDB.Pipeline()

	written := 0
	for _, a := range articles {
		if a.ID == 0 {
			continue
		}
		pipe.ZAdd(fanoutCtx, timelineKey, &redis.Z{
			Score:  float64(a.CreatedAt.Unix()),
			Member: strconv.Itoa(int(a.ID)),
		})
		written++
	}
	if written == 0 {
		return 0, nil
	}

	// 裁剪和 TTL 是「按 key」的操作，整批只需各发一次
	pipe.ZRemRangeByRank(fanoutCtx, timelineKey, 0, -maxItems-1)
	pipe.Expire(fanoutCtx, timelineKey, ttl)

	if _, err := pipe.Exec(fanoutCtx); err != nil {
		return 0, fmt.Errorf("materialize inbox for user %d: %w", userID, err)
	}
	return written, nil
}

// pushToTimelines 分页流式推送
//
// 修复两个问题：
//
//	P3 Pluck 的错误被吞掉：查询失败 → 空切片 → return 0, nil，
//	   看起来推送成功实际一条没推，还不触发熔断。现在错误一路上抛。
//	P4 单次把全部粉丝 ID 拉进内存：1000 万行 = 80MB+ 切片，
//	   实测 10 万行 408ms → 1000 万行约 41 秒且不可中断。
//	   改成按 follower_id 游标分页，内存恒定为一个 batch，且随时可中断。
func (s *BigVFanoutService) pushToTimelines(article models.Article) (pushed int, skippedInactive int, err error) {
	batchSize := s.cfg.Feed.FanoutBatchSize
	if batchSize <= 0 {
		batchSize = 1000
	}
	score := float64(article.CreatedAt.Unix())
	member := strconv.Itoa(int(article.ID))
	maxItems := int64(s.cfg.Feed.TimelineMaxItems)
	if maxItems <= 0 {
		maxItems = 500
	}
	ttlDays := s.cfg.Feed.TimelineTTLDays
	if ttlDays <= 0 {
		ttlDays = 30
	}
	ttl := time.Duration(ttlDays) * 24 * time.Hour
	timeout := time.Duration(s.cfg.Feed.FanoutTimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = time.Second
	}

	resumeAfter := s.loadFanoutProgress(article.ID)
	start := time.Now()

	err = s.streamFollowers(article.AuthorID, resumeAfter, func(batch []uint) error {
		// 维度二：只为活跃读者物化 inbox（§4.3）
		targets, skipped := s.activeOnly(batch)
		skippedInactive += skipped

		if len(targets) > 0 {
			pipe := global.RedisDB.Pipeline()
			for _, fid := range targets {
				timelineKey := fmt.Sprintf("feed:timeline:%d", fid)
				pipe.ZAdd(fanoutCtx, timelineKey, &redis.Z{Score: score, Member: member})
				pipe.ZRemRangeByRank(fanoutCtx, timelineKey, 0, -maxItems-1)
				pipe.Expire(fanoutCtx, timelineKey, ttl)
			}
			if _, perr := pipe.Exec(fanoutCtx); perr != nil {
				return fmt.Errorf("pipeline exec failed at follower batch ending %d: %w", batch[len(batch)-1], perr)
			}
			pushed += len(targets)
		}

		// 记进度：这一批无论下一批会不会超时，都已经**做完了**，
		// 重试必须从这里往后跑，不能回头重做（否则大V永远推不完）。
		s.saveFanoutProgress(article.ID, batch[len(batch)-1])

		// 超时保护：纯保护机制，不是降级触发器。命中就说明这一轮该让路了 ——
		// 返回 fanoutYield 而不是普通错误，重试时按「续跑」而不是「失败」记账。
		if elapsed := time.Since(start); elapsed > timeout {
			return &fanoutYield{processed: pushed, lastID: batch[len(batch)-1]}
		}
		return nil
	})

	if err == nil {
		s.clearFanoutProgress(article.ID)
	}
	return pushed, skippedInactive, err
}

// ==================== 扇出进度（让超时成为可续跑的让路点）====================
//
// 为什么需要它：保护超时按累计耗时算，一个 10 万粉的作者单次必然跑不完。
// 如果超时按「失败」处理、重试又从头开始，这个作者就**永远推不完** ——
// 每轮都在同一处超时，重试次数耗尽后彻底放弃。
// 压测里这个现象非常明确：`FAILED after 1.08s (pushed=0)`，连推都没推到就超时了。
//
// 解法是记进度：超时变成一个**让路点**，存下「推到哪个 follower_id 了」，
// 下一轮从断点续跑（见 fanoutYield / EnqueueResume）。

func (s *BigVFanoutService) progressKey(articleID uint) string {
	return fmt.Sprintf("feed:fanout:progress:%d", articleID)
}

// loadFanoutProgress 读续跑起点，没有就返回 0（从头开始）
func (s *BigVFanoutService) loadFanoutProgress(articleID uint) uint {
	val, err := global.RedisDB.Get(fanoutCtx, s.progressKey(articleID)).Uint64()
	if err != nil {
		return 0
	}
	return uint(val)
}

// TTL 1 小时：扇出本身是分钟级的事，留足重试窗口即可，不必长期驻留
func (s *BigVFanoutService) saveFanoutProgress(articleID uint, lastID uint) {
	global.RedisDB.Set(fanoutCtx, s.progressKey(articleID), lastID, time.Hour)
}

func (s *BigVFanoutService) clearFanoutProgress(articleID uint) {
	global.RedisDB.Del(fanoutCtx, s.progressKey(articleID))
}

// streamFollowers 按 follower_id 游标分页拉取粉丝，逐批回调
//
// 用 keyset pagination 而不是 OFFSET：OFFSET 在深分页时是 O(offset)，
// 1000 万粉丝的第 5000 页要扫 500 万行；keyset 每次都直接从 B+ 树定位。
//
// 依赖索引 idx_follows_fanout (following_id, follower_id, deleted_at)。
// 第三个列不能省 —— GORM 的软删除会补 `deleted_at IS NULL`，少了它优化器会去选
// idx_follows_deleted_at 然后 filesort（实测单批 1000 行 507ms，比旧代码一次
// 拉完 10 万行还慢）。压测把这个坑抓出来了。
//
// startAfter > 0 表示续跑（上一轮让路时停在这里）。
func (s *BigVFanoutService) streamFollowers(authorID uint, startAfter uint, fn func(batch []uint) error) error {
	batchSize := s.cfg.Feed.FanoutBatchSize
	if batchSize <= 0 {
		batchSize = 1000
	}

	lastID := startAfter
	for {
		var batch []uint

		// 用 FORCE INDEX 而不是让优化器自己选 —— 这不是洁癖，是压测逼出来的：
		//
		// 同一个索引，首屏查询（不带 follower_id > ?）它选得对，
		// 一加上游标条件它就切到 idx_follows_deleted_at 上做 filesort
		// （EXPLAIN 实测：rows=48530 / filtered=25% / Using filesort，单批 390ms）。
		// 根因是它以为 `deleted_at IS NULL` 很挑数据，而实际上这张表里
		// 软删除的行极少，那个条件几乎不过滤任何东西 —— 估算错了一个数量级。
		//
		// 代价是这条 SQL 和索引名绑定了，改名要一起改。收益是每一次分页
		// 都确定走有序索引（单批 ~10ms），不再取决于统计信息准不准。
		query := global.Db.Table("follows FORCE INDEX (idx_follows_fanout)").
			Where("following_id = ? AND deleted_at IS NULL", authorID)
		if lastID > 0 {
			query = query.Where("follower_id > ?", lastID)
		}
		// 错误必须上抛：这里是 P3「查询失败伪装成推送成功」的修复点
		if err := query.Order("follower_id ASC").Limit(batchSize).Pluck("follower_id", &batch).Error; err != nil {
			return fmt.Errorf("query followers of author %d after follower_id=%d: %w", authorID, lastID, err)
		}

		if len(batch) == 0 {
			return nil
		}
		if err := fn(batch); err != nil {
			return err
		}

		lastID = batch[len(batch)-1]
		if len(batch) < batchSize {
			return nil
		}
	}
}

// activeOnly 维度二：把一批粉丝过滤成「活跃读者」
//
// 非活跃读者不物化 inbox —— 他们回来时读路径会按需重建。
// 内存成本因此从 O(总粉丝数) 变成 O(DAU)。
//
// 注意：ActiveWindow 的 L1 本地缓存原本没有 TTL 校验（只检查 key 是否存在），
// 用户访问过一次就被永久判为活跃，这个维度会整条失效。
// 已在 utils/active_window.go 修复。
func (s *BigVFanoutService) activeOnly(batch []uint) (active []uint, skipped int) {
	if !s.cfg.Feed.MaterializeActiveOnly || s.activeWin == nil {
		return batch, 0
	}

	inactive := s.activeWin.FilterInactive(batch)
	if len(inactive) == 0 {
		return batch, 0
	}

	inactiveSet := make(map[uint]bool, len(inactive))
	for _, id := range inactive {
		inactiveSet[id] = true
	}

	active = make([]uint, 0, len(batch)-len(inactive))
	for _, id := range batch {
		if !inactiveSet[id] {
			active = append(active, id)
		}
	}
	return active, len(inactive)
}

// GetCircuitBreaker 返回大V注册表里某个作者的熔断器（供外部查询状态）
func (s *BigVFanoutService) GetCircuitBreaker(authorID uint) *FanoutCircuitBreaker {
	return s.breakers.Get(authorID)
}

// Stats 观测用：熔断状态、重试队列深度、异步池积压
func (s *BigVFanoutService) Stats() map[string]any {
	open := s.breakers.Snapshot()
	out := map[string]any{
		"breaker_registry_size": s.breakers.Size(),
		"breakers_not_closed":   open,
		"retry_queue_depth":     s.retryQueue.Depth(),
	}
	if s.pool != nil {
		out["fanout_pool_pending"] = s.pool.Pending()
	}
	return out
}

// getFollowerCount 获取用户粉丝数（带缓存）
//
// 这是路由的唯一输入信号，所以缓存必须有超时：粉丝数变了要能在几分钟内反映出来，
// 否则一个刚涨到 C 类的作者会继续被推给所有粉丝。
func (s *BigVFanoutService) getFollowerCount(userID uint) int64 {
	countKey := fmt.Sprintf("user:%d:follower_count", userID)

	if val, err := global.RedisDB.Get(fanoutCtx, countKey).Int64(); err == nil {
		return val
	}

	var count int64
	if err := global.Db.Model(&models.Follow{}).Where("following_id = ?", userID).Count(&count).Error; err != nil {
		// 查不到粉丝数时保守地当作「大粉丝量」，宁可走拉也不炸写侧
		log.Printf("[BigVFanout] follower count query failed for user %d: %v, assuming large", userID, err)
		return 1 << 40
	}

	global.RedisDB.Set(fanoutCtx, countKey, count, 5*time.Minute)
	return count
}

// GetColdArticles 读路径 Phase 3：冷数据回查
//
// 依然保留 MySQL 版本：C 类作者的近期文章已经由 outbox 归并覆盖（Phase 2.5），
// 但 A/B 类作者在 timeline 窗口之外（被 500 条裁剪掉）的文章只有这里有。
func (s *BigVFanoutService) GetColdArticles(userID uint, followingIDs []uint, limit int, before time.Time) ([]models.Article, error) {
	if len(followingIDs) == 0 {
		return nil, nil
	}

	coldDays := s.cfg.Feed.ColdTimelineTTLDays
	if coldDays <= 0 {
		coldDays = 30
	}
	coldSince := time.Now().Add(-time.Duration(coldDays) * 24 * time.Hour)

	var articles []models.Article
	err := global.Db.Where("author_id IN ? AND status = ? AND created_at BETWEEN ? AND ?",
		followingIDs, "published", coldSince, before).
		Order("created_at DESC").
		Limit(limit).
		Preload("Author").
		Find(&articles).Error

	return articles, err
}
