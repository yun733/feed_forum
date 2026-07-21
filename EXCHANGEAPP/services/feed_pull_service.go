package services

import (
	"context"
	"exchangeapp/global"
	"exchangeapp/models"
	"exchangeapp/utils"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
)

// FeedPullService PULL 模式优化服务
// 解决传统 IN(2000 ids) 查询的性能问题：
//  1. 关注列表缓存在本地内存（1分钟过期）
//  2. 用时间范围扫描替代 IN 查询（利用复合索引）
//  3. 内存中过滤关注列表
//  4. 活跃用户判断走 ActiveWindow（L1 本地 → L2 Redis Pipeline）
type FeedPullService struct {
	localCache  *utils.LocalCache
	activeWin   *utils.ActiveWindow // 可选：L1 本地活跃窗口
}

// NewFeedPullService 创建 PULL 服务
func NewFeedPullService(activeWin *utils.ActiveWindow) *FeedPullService {
	return &FeedPullService{
		localCache: utils.NewLocalCache(10000, 2*time.Minute),
		activeWin:  activeWin,
	}
}

// GetFollowingIDs 获取用户关注列表（带本地缓存）
func (s *FeedPullService) GetFollowingIDs(userID uint) ([]uint, error) {
	cacheKey := fmt.Sprintf("following:%d", userID)

	// Layer 1: 本地内存缓存（热点数据，1分钟有效）
	if cached, ok := s.localCache.Get(cacheKey); ok {
		if ids, ok := cached.([]uint); ok {
			return ids, nil
		}
	}

	// Layer 2: 查 MySQL
	var followingIDs []uint
	err := global.Db.Model(&models.Follow{}).
		Where("follower_id = ?", userID).
		Pluck("following_id", &followingIDs).Error
	if err != nil {
		return nil, err
	}

	// 写入本地缓存
	if len(followingIDs) > 0 {
		s.localCache.Set(cacheKey, followingIDs, 1*time.Minute)
	}

	return followingIDs, nil
}

// InvalidateFollowingCache 失效关注列表缓存（关注/取关时调用）
func (s *FeedPullService) InvalidateFollowingCache(userID uint) {
	cacheKey := fmt.Sprintf("following:%d", userID)
	s.localCache.Delete(cacheKey)
}

// PullArticles 基于时间范围扫描拉取关注者的文章
// 优化策略：
//   - 不使用 WHERE author_id IN (2000 ids) —— 这会产生巨大的 IN 列表
//   - 改用 WHERE status='published' ORDER BY created_at DESC LIMIT N
//   - 在内存中按关注列表过滤，直到凑够 limit 条
//
// 前提：需要 MySQL 复合索引 idx_articles_feed (status, created_at DESC, author_id)
func (s *FeedPullService) PullArticles(userID uint, limit int, excludeIDs []uint) ([]models.Article, error) {
	followingIDs, err := s.GetFollowingIDs(userID)
	if err != nil || len(followingIDs) == 0 {
		return nil, err
	}

	// 构建关注集合用于快速过滤
	followingSet := make(map[uint]bool, len(followingIDs))
	for _, id := range followingIDs {
		followingSet[id] = true
	}

	// 构建排除集合
	excludeSet := make(map[uint]bool, len(excludeIDs))
	for _, id := range excludeIDs {
		excludeSet[id] = true
	}

	// 时间范围扫描：每次取 3 倍 limit 条，内存过滤后尽量凑满
	// 这样避免 IN(2000) 查询，利用 (status, created_at) 复合索引高效扫描
	var result []models.Article
	batchSize := limit * 3
	if batchSize < 100 {
		batchSize = 100
	}

	var cursorTime time.Time // 零值表示从现在开始
	scannedAll := false
	maxScans := 10 // 最多扫10轮防止无限循环

	for len(result) < limit && maxScans > 0 && !scannedAll {
		maxScans--

		var batch []models.Article
		query := global.Db.Where("status = ?", "published")

		// 游标分页：按时间倒序扫描
		if !cursorTime.IsZero() {
			query = query.Where("created_at < ?", cursorTime)
		}

		err := query.
			Order("created_at DESC").
			Limit(batchSize).
			Preload("Author").
			Find(&batch).Error
		if err != nil {
			return result, err
		}

		if len(batch) == 0 {
			scannedAll = true
			break
		}

		// 更新游标到最后一条的时间
		cursorTime = batch[len(batch)-1].CreatedAt

		// 内存过滤：只保留关注者的文章且排除已读
		for _, article := range batch {
			if len(result) >= limit {
				break
			}
			if followingSet[article.AuthorID] && !excludeSet[article.ID] {
				result = append(result, article)
				excludeSet[article.ID] = true
			}
		}

		// 如果取到的数少于 batchSize，说明已经到底了
		if len(batch) < batchSize {
			scannedAll = true
		}
	}

	return result, nil
}

// PullFromInactiveUsers 拉取非活跃用户的文章（优化版）
// 相比原版直接 IN(inactiveUserIDs)，改为按时间扫描 + 内存过滤
func (s *FeedPullService) PullFromInactiveUsers(userID uint, limit int, excludeIDs []uint) ([]models.Article, error) {
	followingIDs, err := s.GetFollowingIDs(userID)
	if err != nil || len(followingIDs) == 0 {
		return nil, err
	}

	// 找出不活跃用户（7天内未活跃）
	// 优先走 ActiveWindow（L1 本地内存命中 → 0 IO，未命中 → L2 Redis Pipeline）
	var inactiveUserIDs []uint
	if s.activeWin != nil {
		inactiveUserIDs = s.activeWin.FilterInactive(followingIDs)
	} else if len(followingIDs) > 0 {
		// 降级：纯 Redis Pipeline
		pipe := global.RedisDB.Pipeline()
		cmds := make([]*redis.IntCmd, len(followingIDs))
		for i, fid := range followingIDs {
			activeKey := fmt.Sprintf("user:active:%d", fid)
			cmds[i] = pipe.Exists(context.Background(), activeKey)
		}
		pipe.Exec(context.Background())
		for i, cmd := range cmds {
			if cmd.Val() == 0 {
				inactiveUserIDs = append(inactiveUserIDs, followingIDs[i])
			}
		}
	}

	if len(inactiveUserIDs) == 0 {
		return nil, nil
	}

	inactiveSet := make(map[uint]bool, len(inactiveUserIDs))
	for _, id := range inactiveUserIDs {
		inactiveSet[id] = true
	}

	excludeSet := make(map[uint]bool, len(excludeIDs))
	for _, id := range excludeIDs {
		excludeSet[id] = true
	}

	// 时间范围扫描
	var result []models.Article
	batchSize := limit * 3
	if batchSize < 100 {
		batchSize = 100
	}

	var cursorTime time.Time
	scannedAll := false
	maxScans := 10

	for len(result) < limit && maxScans > 0 && !scannedAll {
		maxScans--

		var batch []models.Article
		query := global.Db.Where("status = ?", "published")
		if !cursorTime.IsZero() {
			query = query.Where("created_at < ?", cursorTime)
		}

		query.Order("created_at DESC").
			Limit(batchSize).
			Preload("Author").
			Find(&batch)

		if len(batch) == 0 {
			scannedAll = true
			break
		}

		cursorTime = batch[len(batch)-1].CreatedAt

		for _, article := range batch {
			if len(result) >= limit {
				break
			}
			if inactiveSet[article.AuthorID] && !excludeSet[article.ID] {
				result = append(result, article)
				excludeSet[article.ID] = true
			}
		}

		if len(batch) < batchSize {
			scannedAll = true
		}
	}

	return result, nil
}

// GetCache 返回本地缓存（供外部查看统计）
func (s *FeedPullService) GetCache() *utils.LocalCache {
	return s.localCache
}
