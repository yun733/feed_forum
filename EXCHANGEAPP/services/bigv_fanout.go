package services

import (
	"context"
	"exchangeapp/config"
	"exchangeapp/global"
	"exchangeapp/models"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/go-redis/redis/v8"
)

var fanoutCtx = context.Background()

// BigVFanoutService 大V推送服务
// 策略：
//   - 粉丝数 < BigVThreshold：正常 Push 到每个粉丝的 Timeline ZSet
//   - 粉丝数 >= BigVThreshold：走大V通道
//     1. 分批次 Pipeline 写入（防止单次写入过多阻塞Redis）
//     2. 热数据存 Redis（最近7天），冷数据回查 MySQL
//     3. 熔断保护：超时自动降级为全 Pull 模式
type BigVFanoutService struct {
	circuitBreaker *FanoutCircuitBreaker
	cfg            config.Config
}

// NewBigVFanoutService 创建大V推送服务
func NewBigVFanoutService(cfg config.Config) *BigVFanoutService {
	svc := &BigVFanoutService{cfg: cfg}
	svc.circuitBreaker = NewFanoutCircuitBreaker(
		time.Duration(cfg.Feed.FanoutTimeoutMs) * time.Millisecond,
	)
	return svc
}

// GetCircuitBreaker 获取熔断器（供外部查询状态）
func (s *BigVFanoutService) GetCircuitBreaker() *FanoutCircuitBreaker {
	return s.circuitBreaker
}

// FanoutArticle 根据作者粉丝量选择推送策略
// 返回 pushedCount（成功推送数）, isDegraded（是否降级）, error
func (s *BigVFanoutService) FanoutArticle(article models.Article) (pushedCount int, isDegraded bool, err error) {
	// 如果熔断器打开，直接降级
	if !s.circuitBreaker.Allow() {
		log.Printf("[BigVFanout] Circuit breaker OPEN, degrading article %d to pull-only mode", article.ID)
		if err := s.markAsPullOnly(article.ID); err != nil {
			log.Printf("[BigVFanout] Failed to mark article %d as pull-only: %v", article.ID, err)
		}
		return 0, true, nil
	}

	// 获取粉丝数
	followerCount := s.getFollowerCount(article.AuthorID)
	threshold := s.cfg.Feed.BigVThreshold
	if threshold <= 0 {
		threshold = 10000
	}

	if followerCount >= int64(threshold) {
		// 大V通道：分批次推送
		count, err := s.batchFanout(article)
		if err != nil {
			s.circuitBreaker.RecordFailure(err.Error())
			log.Printf("[BigVFanout] Batch fanout failed for big-V article %d: %v, degrading", article.ID, err)
			s.markAsPullOnly(article.ID)
			return 0, true, err
		}
		s.circuitBreaker.RecordSuccess(fanoutCtx, 0)
		log.Printf("[BigVFanout] Big-V article %d fanout completed: %d followers pushed", article.ID, count)
		return count, false, nil
	}

	// 普通用户：正常全量推送
	count, err := s.normalFanout(article)
	if err != nil {
		s.circuitBreaker.RecordFailure(err.Error())
		s.markAsPullOnly(article.ID)
		return 0, true, err
	}
	s.circuitBreaker.RecordSuccess(fanoutCtx, 0)
	return count, false, nil
}

// batchFanout 大V分批次推送（Pipeline 批量写入）
func (s *BigVFanoutService) batchFanout(article models.Article) (int, error) {
	var followerIDs []uint
	global.Db.Model(&models.Follow{}).
		Where("following_id = ?", article.AuthorID).
		Pluck("follower_id", &followerIDs)

	if len(followerIDs) == 0 {
		return 0, nil
	}

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
		ttlDays = 30 // 延长至 30 天，配合互斥锁防止空窗期击穿
	}
	ttl := time.Duration(ttlDays) * 24 * time.Hour

	totalPushed := 0
	startTime := time.Now()

	for i := 0; i < len(followerIDs); i += batchSize {
		end := i + batchSize
		if end > len(followerIDs) {
			end = len(followerIDs)
		}
		batch := followerIDs[i:end]

		pipe := global.RedisDB.Pipeline()
		for _, fid := range batch {
			timelineKey := fmt.Sprintf("feed:timeline:%d", fid)

			// ZAdd 写入文章
			pipe.ZAdd(fanoutCtx, timelineKey, &redis.Z{
				Score:  score,
				Member: member,
			})

			// 保留最近 N 条
			pipe.ZRemRangeByRank(fanoutCtx, timelineKey, 0, -maxItems-1)

			// 设置 TTL（热数据只存最近7天）
			pipe.Expire(fanoutCtx, timelineKey, ttl)
		}

		if _, err := pipe.Exec(fanoutCtx); err != nil {
			return totalPushed, fmt.Errorf("pipeline exec failed at batch %d: %w", i/batchSize, err)
		}

		totalPushed += len(batch)

		// 超时检测：如果已经超过阈值，熔断
		if time.Since(startTime) > time.Duration(s.cfg.Feed.FanoutTimeoutMs)*time.Millisecond {
			return totalPushed, fmt.Errorf("fanout timeout after %d/%d followers", totalPushed, len(followerIDs))
		}
	}

	return totalPushed, nil
}

// normalFanout 普通用户全量推送
func (s *BigVFanoutService) normalFanout(article models.Article) (int, error) {
	return s.batchFanout(article) // 复用相同的 Pipeline 逻辑
}

// markAsPullOnly 标记文章为 Pull-Only 模式（Redis 推送失败时的降级）
func (s *BigVFanoutService) markAsPullOnly(articleID uint) error {
	key := fmt.Sprintf("feed:degraded:%d", articleID)
	// 标记 24 小时，之后可重试
	return global.RedisDB.Set(fanoutCtx, key, "1", 24*time.Hour).Err()
}

// IsDegraded 检查文章是否被降级为 Pull-Only
func (s *BigVFanoutService) IsDegraded(articleID uint) bool {
	key := fmt.Sprintf("feed:degraded:%d", articleID)
	val, _ := global.RedisDB.Get(fanoutCtx, key).Result()
	return val == "1"
}

// RetryDegradedArticle 重试被降级的文章推送
func (s *BigVFanoutService) RetryDegradedArticle(article models.Article) error {
	_, _, err := s.FanoutArticle(article)
	if err == nil {
		// 重试成功，清除降级标记
		key := fmt.Sprintf("feed:degraded:%d", article.ID)
		global.RedisDB.Del(fanoutCtx, key)
	}
	return err
}

// getFollowerCount 获取用户粉丝数（带缓存）
func (s *BigVFanoutService) getFollowerCount(userID uint) int64 {
	countKey := fmt.Sprintf("user:%d:follower_count", userID)

	// 先查 Redis
	val, err := global.RedisDB.Get(fanoutCtx, countKey).Int64()
	if err == nil {
		return val
	}

	// 查 MySQL
	var count int64
	global.Db.Model(&models.Follow{}).Where("following_id = ?", userID).Count(&count)

	// 写缓存（5分钟）
	global.RedisDB.Set(fanoutCtx, countKey, count, 5*time.Minute)

	return count
}

// GetColdArticles 获取冷数据（7-30天前的文章，从 MySQL 回查）
// 当用户的 Timeline 中没有足够的热数据时调用
func (s *BigVFanoutService) GetColdArticles(userID uint, followingIDs []uint, limit int, before time.Time) ([]models.Article, error) {
	if len(followingIDs) == 0 {
		return nil, nil
	}

	coldSince := time.Now().Add(-time.Duration(s.cfg.Feed.ColdTimelineTTLDays) * 24 * time.Hour)
	if coldSince.IsZero() {
		coldSince = time.Now().Add(-30 * 24 * time.Hour)
	}

	var articles []models.Article
	err := global.Db.Where("author_id IN ? AND status = ? AND created_at BETWEEN ? AND ?",
		followingIDs, "published", coldSince, before).
		Order("created_at DESC").
		Limit(limit).
		Preload("Author").
		Find(&articles).Error

	return articles, err
}
