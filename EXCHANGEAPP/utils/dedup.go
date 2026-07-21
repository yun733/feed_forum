package utils

import (
	"context"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
)

const (
	// ReadSetMaxSize 已读集合最大容量
	ReadSetMaxSize = 1000
	// ReadSetTTL 已读集合过期时间
	ReadSetTTL = 7 * 24 * time.Hour
)

// FeedDedup Feed 流去重器
// 三层去重策略：
//  1. 布隆过滤器快速过滤（大概率没看过 → 直接放行）
//  2. Redis Set 精确去重（布隆判"可能看过"时走这个确认）
//  3. 时间戳兜底（同一篇文章的时间戳+作者ID组合唯一，最终屏障）
type FeedDedup struct {
	client *redis.Client
	bloom  *BloomFilter
}

// NewFeedDedup 创建去重器
func NewFeedDedup(client *redis.Client) *FeedDedup {
	return &FeedDedup{
		client: client,
		bloom:  NewBloomFilter(client, "feed:bloom:read"),
	}
}

// IsNewArticle 判断文章对某用户是否为新（未读过）
// 返回 true 表示是新文章，应该展示
func (fd *FeedDedup) IsNewArticle(ctx context.Context, userID uint, articleID uint, createdAt time.Time) (bool, error) {
	// 生成去重标识：articleID + timestamp 组合
	dedupKey := fmt.Sprintf("%d:%d", articleID, createdAt.Unix())

	// Layer 1: 布隆过滤器快速判断
	exists, err := fd.bloom.Exists(ctx, dedupKey)
	if err != nil {
		// 布隆出错时降级：假设没看过，宁可重复不可漏掉
		return true, nil
	}
	if !exists {
		// 布隆说"绝对没看过" → 一定是新的
		return true, nil
	}

	// Layer 2: 布隆说"可能看过" → Redis Set 精确确认
	readSetKey := fmt.Sprintf("feed:read:%d", userID)
	isMember, err := fd.client.SIsMember(ctx, readSetKey, dedupKey).Result()
	if err != nil {
		return true, nil // 降级：Redis 出错时放行
	}
	if !isMember {
		// Set 里没有 → 确认是新的（布隆假阳性）
		return true, nil
	}

	// Layer 3: Set 里也有 → 确实看过了
	return false, nil
}

// MarkAsRead 标记文章为已读
// 同时更新布隆过滤器 + Redis Set
func (fd *FeedDedup) MarkAsRead(ctx context.Context, userID uint, articleID uint, createdAt time.Time) error {
	dedupKey := fmt.Sprintf("%d:%d", articleID, createdAt.Unix())

	// 写入布隆过滤器
	_ = fd.bloom.Add(ctx, dedupKey)

	// 写入 Redis Set
	readSetKey := fmt.Sprintf("feed:read:%d", userID)
	pipe := fd.client.Pipeline()
	pipe.SAdd(ctx, readSetKey, dedupKey)
	// 只保留最近 N 条
	pipe.SCard(ctx, readSetKey)
	pipe.Expire(ctx, readSetKey, ReadSetTTL)

	cmds, err := pipe.Exec(ctx)
	if err != nil {
		return err
	}

	// 超过上限时删除最早的条目
	if len(cmds) >= 2 {
		if card, ok := cmds[1].(*redis.IntCmd); ok {
			if card.Val() > ReadSetMaxSize {
				// 随机移除一半：Set 没有顺序，SPop 是随机出队
				removeCount := ReadSetMaxSize / 2
				for i := 0; i < removeCount; i++ {
					fd.client.SPop(ctx, readSetKey)
				}
			}
		}
	}

	return nil
}

// MarkBatchAsRead 批量标记已读（用于 feed 流返回后标记整页）
func (fd *FeedDedup) MarkBatchAsRead(ctx context.Context, userID uint, items []FeedReadItem) error {
	if len(items) == 0 {
		return nil
	}

	readSetKey := fmt.Sprintf("feed:read:%d", userID)
	pipe := fd.client.Pipeline()

	for _, item := range items {
		dedupKey := fmt.Sprintf("%d:%d", item.ArticleID, item.Timestamp)
		_ = fd.bloom.Add(ctx, dedupKey)
		pipe.SAdd(ctx, readSetKey, dedupKey)
	}

	pipe.Expire(ctx, readSetKey, ReadSetTTL)
	_, err := pipe.Exec(ctx)
	return err
}

// FeedReadItem 去重标记项
type FeedReadItem struct {
	ArticleID uint
	Timestamp int64
}

// ParseArticleID 从去重标识中解析文章 ID
func ParseArticleID(dedupKey string) (uint, error) {
	var articleID uint
	_, err := fmt.Sscanf(dedupKey, "%d:", &articleID)
	return articleID, err
}

// ArticleIDToDedupKey 生成去重标识
func ArticleIDToDedupKey(articleID uint, createdAt time.Time) string {
	return fmt.Sprintf("%d:%d", articleID, createdAt.Unix())
}

// GetReadArticleIDs 获取用户最近已读的文章 ID 列表（用于快速过滤）
func (fd *FeedDedup) GetReadArticleIDs(ctx context.Context, userID uint) (map[uint]bool, error) {
	readSetKey := fmt.Sprintf("feed:read:%d", userID)
	members, err := fd.client.SMembers(ctx, readSetKey).Result()
	if err != nil {
		return nil, err
	}

	result := make(map[uint]bool, len(members))
	for _, m := range members {
		var articleID uint
		fmt.Sscanf(m, "%d:", &articleID)
		if articleID > 0 {
			result[articleID] = true
		}
	}
	return result, nil
}

// BloomFilter 增强版布隆过滤器（导出供外部使用）
// 原始实现保留，此方法为去重器内部使用
func (fd *FeedDedup) GetBloomFilter() *BloomFilter {
	return fd.bloom
}

// ResetUserReadHistory 重置用户阅读历史（如用户请求"清除历史"）
func (fd *FeedDedup) ResetUserReadHistory(ctx context.Context, userID uint) error {
	readSetKey := fmt.Sprintf("feed:read:%d", userID)
	return fd.client.Del(ctx, readSetKey).Err()
}
