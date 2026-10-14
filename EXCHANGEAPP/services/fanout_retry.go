package services

import (
	"errors"
	"exchangeapp/global"
	"exchangeapp/models"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
)

// 推送失败重试队列 —— 替代原来的 feed:degraded:<id> 死代码标记（P5）
//
// 修复前：推送失败时写 `feed:degraded:<id>` 标记 24 小时，
//        但全仓库没有任何地方读它，RetryDegradedArticle 也是零调用。
//        用户最终还能看到文章，靠的是冷数据回查「恰好没有过滤这个标记」——
//        是巧合不是设计。
//
// 修复后：失败的文章进一个**可索引**的 ZSet（score = 下次重试时间），
//        后台 worker 到点捞出来重推。重试用指数退避，超过上限告警。
//
// 为什么用 ZSet 而不是 `KEYS feed:degraded:*`：Redis 的 KEYS 是 O(N) 全库扫描，
//        生产环境等同于故障；ZRANGEBYSCORE 是 O(log N + M)。

const fanoutRetryKey = "feed:fanout:retry"

// fanoutYield 表示「这一轮到点了，进度已存，下一轮接着跑」——不是失败
//
// 和普通错误分开记的原因：普通错误要消耗重试次数（错误重复出现说明有问题），
// 而让路是**正常的分片推进**，一个 1000 万粉的作者可能要几十轮才推完，
// 消耗次数的话会推到中途就被判死刑。让路永远有进展（每次至少推进一个 batch），
// 所以不设次数上限也不会死循环。
type fanoutYield struct {
	processed int
	lastID    uint
}

func (e *fanoutYield) Error() string {
	return fmt.Sprintf("fanout yielded after %d followers at id=%d (progress saved, will resume)",
		e.processed, e.lastID)
}

// IsFanoutYield 判断错误是不是「让路」（供重试 worker 决定怎么记账）
func IsFanoutYield(err error) bool {
	var y *fanoutYield
	return errors.As(err, &y)
}

// RetryItem 重试队列里的一条
type RetryItem struct {
	ArticleID uint
	Attempt   int
	Member    string // 原样回传，删除时用（articleID:attempt）
}

// FanoutRetryQueue 待重试文章队列
type FanoutRetryQueue struct {
	maxRetries int
}

// NewFanoutRetryQueue 创建重试队列
func NewFanoutRetryQueue(maxRetries int) *FanoutRetryQueue {
	if maxRetries <= 0 {
		maxRetries = 3
	}
	return &FanoutRetryQueue{maxRetries: maxRetries}
}

// Enqueue 把文章加入重试队列（指数退避：2s, 4s, 8s, ... 封顶 5 分钟）
func (q *FanoutRetryQueue) Enqueue(articleID uint, attempt int, reason string) {
	if attempt >= q.maxRetries {
		// 超过上限：不再静默重试，打一条明确的告警。
		// 此时文章仍然能被读到 —— 读路径 Phase 3 的 MySQL 冷查询按
		// author_id IN (followingIDs) 回查，不依赖任何标记。
		log.Printf("[FanoutRetry] ALERT article %d exhausted %d retries (%s); "+
			"still reachable via pull fallback, manual inspection recommended",
			articleID, attempt, reason)
		return
	}

	delay := time.Duration(1<<uint(attempt)) * 2 * time.Second
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	member := fmt.Sprintf("%d:%d", articleID, attempt+1)
	next := time.Now().Add(delay)

	if err := global.RedisDB.ZAdd(fanoutCtx, fanoutRetryKey, &redis.Z{Score: float64(next.Unix()), Member: member}).Err(); err != nil {
		log.Printf("[FanoutRetry] failed to enqueue article %d: %v", articleID, err)
	}
}

// EnqueueResume 续跑入队：**不推进 attempt**，因此不受重试次数上限约束
//
// member 保持和取出来时同一个编号，也就是「这次让路不算一次失败」。
// 只有 fanoutYield 走这条路 —— 它保证每次至少推进一个 batch，不会死循环。
func (q *FanoutRetryQueue) EnqueueResume(articleID uint, attempt int) {
	if attempt < 1 {
		attempt = 1
	}
	member := fmt.Sprintf("%d:%d", articleID, attempt)
	next := time.Now().Add(resumeDelay)

	if err := global.RedisDB.ZAdd(fanoutCtx, fanoutRetryKey,
		&redis.Z{Score: float64(next.Unix()), Member: member}).Err(); err != nil {
		log.Printf("[FanoutRetry] failed to re-enqueue article %d for resume: %v", articleID, err)
	}
}

// resumeDelay 让路后多久续跑。比首次重试的 2s 短 —— 让路不是故障，
// 只是把一次长任务切成几段，没必要退避。
const resumeDelay = 1 * time.Second

// Due 取出到点的重试项
func (q *FanoutRetryQueue) Due(limit int) []RetryItem {
	if limit <= 0 {
		limit = 50
	}
	res, err := global.RedisDB.ZRangeByScore(fanoutCtx, fanoutRetryKey, &redis.ZRangeBy{
		Min:   "-inf",
		Max:   strconv.FormatInt(time.Now().Unix(), 10),
		Count: int64(limit),
	}).Result()
	if err != nil {
		return nil
	}

	items := make([]RetryItem, 0, len(res))
	for _, m := range res {
		parts := strings.SplitN(m, ":", 2)
		if len(parts) != 2 {
			continue
		}
		id, err := strconv.ParseUint(parts[0], 10, 64)
		if err != nil {
			continue
		}
		attempt, _ := strconv.Atoi(parts[1])
		items = append(items, RetryItem{ArticleID: uint(id), Attempt: attempt, Member: m})
	}
	return items
}

// Complete 重试成功，从队列移除
func (q *FanoutRetryQueue) Complete(item RetryItem) {
	global.RedisDB.ZRem(fanoutCtx, fanoutRetryKey, item.Member)
}

// Depth 队列深度（供观测）
func (q *FanoutRetryQueue) Depth() int64 {
	n, _ := global.RedisDB.ZCard(fanoutCtx, fanoutRetryKey).Result()
	return n
}

// ==================== 后台重试 worker ====================

// FanoutRetryWorker 定时扫描重试队列并重推
type FanoutRetryWorker struct {
	queue        *FanoutRetryQueue
	scanInterval time.Duration
	stopCh       chan struct{}
	stopped      chan struct{}
	// retry 真正执行重推的回调（由 BigVFanoutService 提供，避免反向依赖）
	retry func(article models.Article) error
}

// NewFanoutRetryWorker 创建重试 worker
func NewFanoutRetryWorker(queue *FanoutRetryQueue, scanInterval time.Duration, retry func(models.Article) error) *FanoutRetryWorker {
	if scanInterval <= 0 {
		scanInterval = 10 * time.Second
	}
	return &FanoutRetryWorker{
		queue:        queue,
		scanInterval: scanInterval,
		stopCh:       make(chan struct{}),
		stopped:      make(chan struct{}),
		retry:        retry,
	}
}

// Start 启动定时扫描
func (w *FanoutRetryWorker) Start() {
	go func() {
		ticker := time.NewTicker(w.scanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.processBatch()
			case <-w.stopCh:
				return
			}
		}
	}()
	log.Printf("[FanoutRetry] Worker started, scan interval %v", w.scanInterval)
}

// Stop 停止
func (w *FanoutRetryWorker) Stop() {
	select {
	case <-w.stopped:
		return
	default:
		close(w.stopped)
		close(w.stopCh)
	}
}

func (w *FanoutRetryWorker) processBatch() {
	items := w.queue.Due(50)
	if len(items) == 0 {
		return
	}

	for _, item := range items {
		var article models.Article
		if err := global.Db.Preload("Author").First(&article, item.ArticleID).Error; err != nil {
			// 文章被删了：直接从队列移除，别一直重试
			log.Printf("[FanoutRetry] article %d not found, dropping from retry queue: %v", item.ArticleID, err)
			w.queue.Complete(item)
			continue
		}

		if err := w.retry(article); err != nil {
			w.queue.Complete(item)
			if IsFanoutYield(err) {
				// 让路：续跑，不消耗重试次数
				w.queue.EnqueueResume(article.ID, item.Attempt)
				continue
			}
			// 真失败：重新入队时 attempt 已经 +1（Enqueue 内部处理）
			w.queue.Enqueue(article.ID, item.Attempt, err.Error())
			continue
		}
		w.queue.Complete(item)
		log.Printf("[FanoutRetry] article %d retried successfully on attempt %d", article.ID, item.Attempt)
	}
}
