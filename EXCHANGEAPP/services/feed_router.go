package services

import (
	"exchangeapp/config"
	"fmt"
)

// 推拉分层路由（§4.2）
//
// 设计要点：决策信号从「运行时耗时」换成 followerCount。
//
//	改造前：推给所有粉丝，超过 fanout_timeout_ms 就中止并降级 —— 决策信号是
//	        「当时 Redis 有多忙」，同一篇文章两次发布结果可能不同，
//	        用户 A 看到、用户 B 没看到，不可复现、无法解释。
//	改造后：Classify/Decide 是纯函数，输入只有粉丝数，输出恒定。
//	        超时保留，但只作为「防单条消息永久阻塞消费者」的保护机制，
//	        命中即告警，不再静默换策略。
//
// 这三个类型/函数不碰 Redis、不碰 MySQL、不碰时间，所以可以直接单测。

// AuthorTier 作者按粉丝量的分层
type AuthorTier int

const (
	TierA AuthorTier = iota // 中小作者：同步推
	TierB                   // 中大型作者：异步延迟推
	TierC                   // 大V：不推，只写 outbox
)

func (t AuthorTier) String() string {
	switch t {
	case TierA:
		return "A"
	case TierB:
		return "B"
	case TierC:
		return "C"
	default:
		return "UNKNOWN"
	}
}

// PushStrategy 写侧要做多少
type PushStrategy int

const (
	// StrategySyncPush 同步推给粉丝，阻塞发文/消费主流程
	StrategySyncPush PushStrategy = iota
	// StrategyAsyncPush 异步延迟推，不阻塞主流程
	StrategyAsyncPush
	// StrategyPullOnly 完全不推，只写作者自己的 outbox，粉丝读时归并
	StrategyPullOnly
)

func (s PushStrategy) String() string {
	switch s {
	case StrategySyncPush:
		return "SYNC_PUSH"
	case StrategyAsyncPush:
		return "ASYNC_PUSH"
	case StrategyPullOnly:
		return "PULL_ONLY"
	default:
		return "UNKNOWN"
	}
}

// RouteDecision 一次路由的完整结果，用于日志/指标/断言
type RouteDecision struct {
	Tier          AuthorTier
	Strategy      PushStrategy
	FollowerCount int64
	// Threshold 本次命中的分界线，方便排查「为什么这篇文章走了这个策略」
	Threshold int64
}

// FeedRouter 按粉丝量做确定性路由
type FeedRouter struct {
	tierAMax int64
	tierCMin int64
}

// NewFeedRouter 从配置构造路由器，非法配置回落到设计文档的建议值
func NewFeedRouter(cfg config.Config) *FeedRouter {
	tierAMax := int64(cfg.Feed.TierAMaxFollowers)
	if tierAMax <= 0 {
		tierAMax = 5000
	}
	tierCMin := int64(cfg.Feed.TierCMinFollowers)
	if tierCMin <= 0 {
		tierCMin = 500000
	}
	if tierCMin < tierAMax {
		tierCMin = tierAMax
	}
	return &FeedRouter{tierAMax: tierAMax, tierCMin: tierCMin}
}

// Classify 纯函数：粉丝量 → 层级。相同输入永远得到相同输出。
func (r *FeedRouter) Classify(followerCount int64) AuthorTier {
	switch {
	case followerCount >= r.tierCMin:
		return TierC
	case followerCount >= r.tierAMax:
		return TierB
	default:
		return TierA
	}
}

// Decide 纯函数：粉丝量 → 路由决策
func (r *FeedRouter) Decide(followerCount int64) RouteDecision {
	tier := r.Classify(followerCount)

	// 阈值只用于日志/指标，不参与判断 —— 这样「为什么走了这条路径」随时可复现
	var threshold int64
	switch tier {
	case TierC:
		threshold = r.tierCMin
	case TierB:
		threshold = r.tierAMax
	default:
		threshold = r.tierAMax
	}

	var strategy PushStrategy
	switch tier {
	case TierC:
		// 推的边际收益已归零：1000 万粉要写 3000 万条命令 + 414GB，
		// 而 outbox 只要 1 条 ZADD
		strategy = StrategyPullOnly
	case TierB:
		strategy = StrategyAsyncPush
	default:
		strategy = StrategySyncPush
	}

	return RouteDecision{
		Tier:          tier,
		Strategy:      strategy,
		FollowerCount: followerCount,
		Threshold:     threshold,
	}
}

// InboxesWithinBudget 由内存预算反推「可物化 inbox 上限」（§4.6）
//
//	N = 可用内存字节数 ÷ 单时间线字节数
//
// 本机实测单时间线 44,558 字节（500 条上限），32GB → 约 70 万。
// 这个数字是阈值推导的起点：T2 取在「推的边际内存成本 = 拉的边际读成本」的交叉点，
// 再结合 DAU 调整 —— 因为只有活跃用户才真的占内存。
func InboxesWithinBudget(budgetMB int, bytesPerUser int) int64 {
	if budgetMB <= 0 || bytesPerUser <= 0 {
		return 0
	}
	return int64(budgetMB) * 1024 * 1024 / int64(bytesPerUser)
}

// Describe 给日志/接口用的一行摘要
func (d RouteDecision) Describe(articleID uint) string {
	return fmt.Sprintf("article=%d tier=%s strategy=%s followers=%d threshold=%d",
		articleID, d.Tier, d.Strategy, d.FollowerCount, d.Threshold)
}
