package services

import (
	"exchangeapp/config"
	"exchangeapp/global"
	"exchangeapp/models"
	"exchangeapp/utils"
	"fmt"
	"log"
	"sort"
	"strconv"
	"time"

	"github.com/go-redis/redis/v8"
)

// C 类作者 outbox（§4.4）
//
// 数据结构：
//
//	author:outbox:<authorID>   ZSet，score = 发文时间戳，member = articleID
//	feed:authors:c             Set，当前处于 C 类（大V）的作者 ID
//
// 关键区别：outbox 是**作者维度、全局共享**的 —— 一个 1000 万粉的账号，
// Redis 里就只有 author:outbox:<id> 这一个 key，而不是 1000 万份 timeline。
//
// 和 timeline 的关系：
//
//	timeline = 每个读者一个（feed:timeline:<uid>），装 A/B 类作者推给他的文章
//	outbox   = 每个 C 类作者一个，装该大V自己发的文章
//
// 两者内容不相交：一篇文章要么被推（进 N 个粉丝的 timeline），要么没被推
// （只进作者自己的 outbox）。所以读侧是「1 个 timeline + K 个 outbox」的归并。

const (
	// authorOutboxKeyPrefix author:outbox:<authorID>
	authorOutboxKeyPrefix = "author:outbox:"
	// tierCAuthorSetKey 当前 C 类作者集合
	tierCAuthorSetKey = "feed:authors:c"
	// outboxHeadCacheKeyPrefix 大V outbox 头部的本地缓存前缀（§4.5 第一道防线）
	outboxHeadCacheKeyPrefix = "outbox:head:"
)

// OutboxEntry outbox 里的一条记录：文章 ID + 排序用的 score
type OutboxEntry struct {
	ArticleID uint
	Score     int64
}

// AuthorOutboxService C 类作者的发文 outbox
type AuthorOutboxService struct {
	cfg config.Config
	// headCache 缓存每个大V outbox 的「最近 N 条」。
	// 一个大V发文章后 500 万粉丝陆续来读，每人一次 ZRANGEBYSCORE 就是 500 万次读尖峰；
	// 头部加本地缓存后，命中即 0 网络 IO（和 ActiveWindow 的 L1 是同一个模式）。
	headCache *utils.LocalCache

	ttl      time.Duration
	maxItems int64
	fetchLim int64
	maxBigVs int
	setTTL   time.Duration
}

// NewAuthorOutboxService 创建 outbox 服务
func NewAuthorOutboxService(cfg config.Config) *AuthorOutboxService {
	ttlDays := cfg.Feed.OutboxTTLDays
	if ttlDays <= 0 {
		ttlDays = 30
	}
	maxItems := cfg.Feed.OutboxMaxItems
	if maxItems <= 0 {
		maxItems = 500
	}
	fetchLim := cfg.Feed.OutboxFetchLimit
	if fetchLim <= 0 {
		fetchLim = 50
	}
	maxBigVs := cfg.Feed.MaxTierCAuthorsPerUser
	if maxBigVs <= 0 {
		maxBigVs = 100
	}
	setTTL := time.Duration(cfg.Feed.TierCSetCacheTTLSeconds) * time.Second
	if setTTL <= 0 {
		setTTL = 60 * time.Second
	}

	return &AuthorOutboxService{
		cfg:       cfg,
		headCache: utils.NewLocalCache(5000, 30*time.Second),
		ttl:       time.Duration(ttlDays) * 24 * time.Hour,
		maxItems:  int64(maxItems),
		fetchLim:  int64(fetchLim),
		maxBigVs:  maxBigVs,
		setTTL:    setTTL,
	}
}

// Append 把文章写入作者自己的 outbox（C 类作者发文时调用）
//
// 写侧成本 O(1)：1 次 Pipeline（ZADD + 裁剪 + EXPIRE），
// 对比推模式给 1000 万粉丝写 3000 万条命令。
func (s *AuthorOutboxService) Append(article models.Article) error {
	key := fmt.Sprintf("%s%d", authorOutboxKeyPrefix, article.AuthorID)

	pipe := global.RedisDB.Pipeline()
	pipe.ZAdd(fanoutCtx, key, &redis.Z{
		Score:  float64(article.CreatedAt.Unix()),
		Member: strconv.Itoa(int(article.ID)),
	})
	pipe.ZRemRangeByRank(fanoutCtx, key, 0, -s.maxItems-1)
	pipe.Expire(fanoutCtx, key, s.ttl)
	if _, err := pipe.Exec(fanoutCtx); err != nil {
		return fmt.Errorf("append outbox for author %d: %w", article.AuthorID, err)
	}

	// 头部缓存就地失效：下一个读者会重建它，拿到刚发的这条
	s.headCache.Delete(fmt.Sprintf("%s%d", outboxHeadCacheKeyPrefix, article.AuthorID))
	return nil
}

// RegisterTierC 把作者标记为 C 类（大V）
//
// 迁移兼容（§5.1）：只登记作者身份，**不回灌历史**。
// 回灌会让同一篇文章同时存在于粉丝的 timeline（历史推送）和作者的 outbox，
// 归并时读两遍 —— 那正是「重复」风险的来源。
// 不回灌没有任何损失：timeline 本来就有 500 条裁剪，切换后新文章照常可见。
//
// 调用顺序要求「先在写侧建 outbox → 再切路由」，见 BigVFanoutService.FanoutArticle：
// 那里是先 Append 再登记，保证切换窗口里文章不会两边都没有。
//
// 登记后必须失效本地缓存：否则刚才缓存下来的那份「还没有大V」会继续生效 60 秒，
// 这期间新登记的大V的文章对所有读者都不可见 —— 文章没丢，但没人看得见。
func (s *AuthorOutboxService) RegisterTierC(authorID uint) error {
	if err := global.RedisDB.SAdd(fanoutCtx, tierCAuthorSetKey, authorID).Err(); err != nil {
		return err
	}
	s.headCache.Delete(tierCAuthorSetKey)
	return nil
}

// IsTierC 作者当前是否被登记为 C 类
//
// 读的是本地缓存的集合（60s TTL），所以是一次内存查找，不会给每次发文
// 增加一次 Redis 往返。误判的代价只是多/少一次 SREM，是幂等的。
func (s *AuthorOutboxService) IsTierC(authorID uint) bool {
	return s.tierCAuthorSet()[authorID]
}

// UnregisterTierC 作者退出 C 类（粉丝量掉下来了 / 阈值调整）
//
// 自愈：退出的那一刻起新文章走推，老文章仍在 outbox 里。
// 理论上两边可能同时有同一篇文章（切换窗口），但读侧有 FeedDedup 三层去重兜底。
//
// 同样要失效本地缓存：不失效的话读路径还会继续归并一个已经不该被归并的 outbox，
// 结果是同一篇文章既被推、又被归并 —— 去重能兜住，但那是浪费。
func (s *AuthorOutboxService) UnregisterTierC(authorID uint) error {
	if err := global.RedisDB.SRem(fanoutCtx, tierCAuthorSetKey, authorID).Err(); err != nil {
		return err
	}
	s.headCache.Delete(tierCAuthorSetKey)
	return nil
}

// tierCAuthorSet 取「当前所有 C 类作者」
//
// 这个集合天然很小 —— C 类按定义就是极少数作者，所以 SMEMBERS 可以直接拉全量，
// 再在本地内存里和用户的关注列表求交集，不需要逐 ID 去 Redis 问 SISMEMBER。
func (s *AuthorOutboxService) tierCAuthorSet() map[uint]bool {
	cacheKey := tierCAuthorSetKey
	if cached, ok := s.headCache.Get(cacheKey); ok {
		if set, ok := cached.(map[uint]bool); ok {
			return set
		}
	}

	members, err := global.RedisDB.SMembers(fanoutCtx, tierCAuthorSetKey).Result()
	if err != nil {
		// Redis 故障降级：当作没有大V → 读路径退回 MySQL 冷查询，不返回错误
		log.Printf("[AuthorOutbox] SMEMBERS %s failed: %v", tierCAuthorSetKey, err)
		return map[uint]bool{}
	}

	set := make(map[uint]bool, len(members))
	for _, m := range members {
		id, err := strconv.ParseUint(m, 10, 64)
		if err != nil {
			continue
		}
		set[uint(id)] = true
	}

	s.headCache.Set(cacheKey, set, s.setTTL)
	return set
}

// FollowedTierCAuthors 从用户的关注列表里筛出 C 类作者，并施加 K 的硬上限
//
// K 的代价是线性的：读一次 Feed 要多读 K 个 outbox。虽然能 pipeline 成 1 次 RTT，
// 但命令数还是随 K 增长，所以必须有上限（§8）。
func (s *AuthorOutboxService) FollowedTierCAuthors(followingIDs []uint) []uint {
	if len(followingIDs) == 0 {
		return nil
	}
	set := s.tierCAuthorSet()
	if len(set) == 0 {
		return nil
	}

	out := make([]uint, 0, 8)
	for _, id := range followingIDs {
		if set[id] {
			out = append(out, id)
			if len(out) >= s.maxBigVs {
				log.Printf("[AuthorOutbox] user follows %d+ tier-C authors, truncated to K=%d",
					s.maxBigVs, s.maxBigVs)
				break
			}
		}
	}
	return out
}

// HeadBatch 一次读取的第一阶段：把作者分成「本地缓存命中」和「需要问 Redis」两组
//
// 缓存粒度必须是**单作者**而不是「用户关注的大V集合」—— 否则某个大V发一篇文章
// 就要清掉所有人的缓存，而清缓存的那一刻正好是 500 万粉丝的读尖峰打进来的时候。
// 按作者缓存时，Append 只失效自己那一个 key，其余 K-1 个作者的缓存仍然热着。
//
// useCache 只在首屏（无游标）时为 true：翻页相对少，且必须读实时数据。
func (s *AuthorOutboxService) HeadBatch(authorIDs []uint, useCache bool) (cached map[uint][]OutboxEntry, misses []uint) {
	if len(authorIDs) == 0 {
		return nil, nil
	}
	cached = make(map[uint][]OutboxEntry, len(authorIDs))
	for _, aid := range authorIDs {
		if useCache {
			if entries, ok := s.cachedHead(aid); ok {
				cached[aid] = entries
				continue
			}
		}
		misses = append(misses, aid)
	}
	return cached, misses
}

// QueueFetch 把 K 个 outbox 的读取命令挂到**调用方已有的 pipeline**上
//
// 这是「多查一个 outbox 是不是白多一次往返」的答案（§4.4.1）：不是。
// 一次 Feed 读本来就要从 Redis 拿已读集合 + 自己的 timeline，现在把 K 个 outbox
// 也塞进同一次 pipeline，K 条 ZREVRANGEBYSCORE 搭在同一次 RTT 上走。
// 压测显示瓶颈是往返次数不是命令数，所以边际成本基本为零。
func (s *AuthorOutboxService) QueueFetch(pipe redis.Pipeliner, authorIDs []uint, before int64) []*redis.ZSliceCmd {
	max := "+inf"
	if before > 0 {
		// 和 timeline 的游标语义保持一致（feed_controller.go 的 maxScore = cursor.Score - 1）
		max = strconv.FormatInt(before-1, 10)
	}

	cmds := make([]*redis.ZSliceCmd, len(authorIDs))
	for i, aid := range authorIDs {
		key := fmt.Sprintf("%s%d", authorOutboxKeyPrefix, aid)
		cmds[i] = pipe.ZRevRangeByScoreWithScores(fanoutCtx, key, &redis.ZRangeBy{
			Min:    "-inf",
			Max:    max,
			Offset: 0,
			Count:  s.fetchLim,
		})
	}
	return cmds
}

// CollectFetch 解析 QueueFetch 挂进去的命令，并把结果写回本地缓存
// 返回值与 authorIDs 一一对应
func (s *AuthorOutboxService) CollectFetch(authorIDs []uint, cmds []*redis.ZSliceCmd, useCache bool) [][]OutboxEntry {
	out := make([][]OutboxEntry, len(authorIDs))
	for i, cmd := range cmds {
		if cmd == nil {
			continue
		}
		zs, err := cmd.Result()
		if err != nil {
			// 单个 key 不存在时 Redis 返回空数组而不是错误，所以这里基本都是真故障
			continue
		}
		entries := make([]OutboxEntry, 0, len(zs))
		for _, z := range zs {
			id, err := strconv.ParseUint(fmt.Sprint(z.Member), 10, 64)
			if err != nil {
				continue
			}
			entries = append(entries, OutboxEntry{ArticleID: uint(id), Score: int64(z.Score)})
		}
		out[i] = entries
		if useCache {
			s.headCache.Set(headCacheKey(authorIDs[i]), entries, outboxHeadTTL)
		}
	}
	return out
}

// FetchRecent 读 K 个大V的 outbox，归并成按时间倒序、已去重的条目
//
// 这是 HeadBatch + QueueFetch + CollectFetch 的便利封装，自带一次 pipeline。
// 读路径（GetFeed）不走这个函数 —— 它会把 QueueFetch 挂到自己的 pipeline 上，
// 省掉这一路往返。
func (s *AuthorOutboxService) FetchRecent(authorIDs []uint, before int64) []OutboxEntry {
	if len(authorIDs) == 0 {
		return nil
	}

	useCache := before <= 0
	cached, misses := s.HeadBatch(authorIDs, useCache)
	fetched := map[uint][]OutboxEntry{}
	if len(misses) > 0 {
		pipe := global.RedisDB.Pipeline()
		cmds := s.QueueFetch(pipe, misses, before)
		if _, err := pipe.Exec(fanoutCtx); err != nil {
			log.Printf("[AuthorOutbox] pipeline fetch failed for %d authors: %v", len(misses), err)
		} else {
			results := s.CollectFetch(misses, cmds, useCache)
			for i, aid := range misses {
				fetched[aid] = results[i]
			}
		}
	}

	merged := make([]OutboxEntry, 0, len(authorIDs)*4)
	seen := make(map[uint]bool, len(authorIDs)*4)
	for _, aid := range authorIDs {
		entries := cached[aid]
		if e, ok := fetched[aid]; ok {
			entries = e
		}
		for _, entry := range entries {
			if seen[entry.ArticleID] {
				continue
			}
			seen[entry.ArticleID] = true
			merged = append(merged, entry)
		}
	}

	sort.Slice(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })
	return merged
}

func headCacheKey(authorID uint) string {
	return fmt.Sprintf("%s%d", outboxHeadCacheKeyPrefix, authorID)
}

func (s *AuthorOutboxService) cachedHead(authorID uint) ([]OutboxEntry, bool) {
	cached, ok := s.headCache.Get(headCacheKey(authorID))
	if !ok {
		return nil, false
	}
	entries, ok := cached.([]OutboxEntry)
	return entries, ok
}

// HasTierCAuthors 系统里当前是否有 C 类作者
//
// 用来做零成本短路：没有大V（比如刚部署、或者测试数据里没有大V）时，
// 读路径完全不去查关注列表，新增的归并逻辑开销严格为 0。
func (s *AuthorOutboxService) HasTierCAuthors() bool {
	return len(s.tierCAuthorSet()) > 0
}

// headCacheTTL 头部缓存存活时间。
// 可以放心取小值：Append 会精确失效对应作者的 key，所以缓存不会「看不见新文章」，
// TTL 只是兜底（防止某个大V被取关后缓存常驻）。
const outboxHeadTTL = 15 * time.Second
