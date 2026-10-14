package services

import (
	"exchangeapp/config"
	"testing"
)

func testRouter(tierAMax, tierCMin int) *FeedRouter {
	var cfg config.Config
	cfg.Feed.TierAMaxFollowers = tierAMax
	cfg.Feed.TierCMinFollowers = tierCMin
	return NewFeedRouter(cfg)
}

// Classify/Decide 是纯函数：这是整个改造的可测试性收益所在。
// 改造前「超时降级」只能靠起 Redis + MySQL 观察日志，几乎没法断言；
// 现在路由决策不碰任何 IO，可以直接列边界用例。
func TestClassifyBoundaries(t *testing.T) {
	r := testRouter(5000, 500000)

	cases := []struct {
		followers int64
		want      AuthorTier
	}{
		{0, TierA},
		{1, TierA},
		{4999, TierA},
		{5000, TierB}, // 边界：含下界
		{5001, TierB},
		{499999, TierB},
		{500000, TierC}, // 边界：含下界
		{500001, TierC},
		{10_000_000, TierC},
	}

	for _, c := range cases {
		if got := r.Classify(c.followers); got != c.want {
			t.Errorf("Classify(%d) = %s, want %s", c.followers, got, c.want)
		}
	}
}

func TestDecideStrategy(t *testing.T) {
	r := testRouter(5000, 500000)

	cases := []struct {
		followers int64
		tier      AuthorTier
		strategy  PushStrategy
	}{
		{100, TierA, StrategySyncPush},
		{4999, TierA, StrategySyncPush},
		{5000, TierB, StrategyAsyncPush},
		{499999, TierB, StrategyAsyncPush},
		{500000, TierC, StrategyPullOnly},
		{10_000_000, TierC, StrategyPullOnly},
	}

	for _, c := range cases {
		d := r.Decide(c.followers)
		if d.Tier != c.tier {
			t.Errorf("Decide(%d).Tier = %s, want %s", c.followers, d.Tier, c.tier)
		}
		if d.Strategy != c.strategy {
			t.Errorf("Decide(%d).Strategy = %s, want %s", c.followers, d.Strategy, c.strategy)
		}
		if d.FollowerCount != c.followers {
			t.Errorf("Decide(%d).FollowerCount = %d", c.followers, d.FollowerCount)
		}
	}
}

// 这是整个改造的核心断言：路由结果只取决于粉丝量，不取决于时机。
// 改造前决策信号是「耗时超没超 1 秒」，同一篇文章两次发布可能一次推完、
// 一次推一半，用户 A 看到、用户 B 没看到 —— 不可复现、无法解释。
func TestDecideIsDeterministic(t *testing.T) {
	r := testRouter(5000, 500000)

	for _, followers := range []int64{0, 4999, 5000, 499999, 500000, 10_000_000} {
		first := r.Decide(followers)
		for i := 0; i < 100; i++ {
			if got := r.Decide(followers); got != first {
				t.Fatalf("Decide(%d) not deterministic: %+v vs %+v", followers, got, first)
			}
		}
	}
}

// 非法配置要有安全回落，不能因为一个笔误让所有作者都走 C 类（文章就没人推了）
func TestRouterConfigFallback(t *testing.T) {
	r := testRouter(0, 0)
	if r.tierAMax != 5000 || r.tierCMin != 500000 {
		t.Errorf("zero config should fall back to defaults, got t1=%d t2=%d", r.tierAMax, r.tierCMin)
	}

	// t2 < t1 是矛盾配置：压成相等，B 类区间退化为空，但 A/C 的划分依然明确
	// （宁可让 B 类消失，也不能出现「既是 B 又是 C」这种路由结果不唯一的区间）
	r2 := testRouter(10000, 5000)
	if r2.tierCMin < r2.tierAMax {
		t.Errorf("t2 should be clamped to >= t1, got t1=%d t2=%d", r2.tierAMax, r2.tierCMin)
	}
	if got := r2.Classify(9999); got != TierA {
		t.Errorf("Classify(9999) with t1=t2=10000 = %s, want A", got)
	}
	if got := r2.Classify(10000); got != TierC {
		t.Errorf("Classify(10000) with t1=t2=10000 = %s, want C", got)
	}
}

// 阈值反推公式（§4.6）：N = 内存预算 ÷ 单时间线字节数
func TestInboxesWithinBudget(t *testing.T) {
	// 本机实测：32GB 可用内存 / 44,558 字节每个 timeline ≈ 70 万
	got := InboxesWithinBudget(32768, 44558)
	if got < 700_000 || got > 780_000 {
		t.Errorf("InboxesWithinBudget(32768, 44558) = %d, want ~700k", got)
	}

	if n := InboxesWithinBudget(0, 44558); n != 0 {
		t.Errorf("zero budget should yield 0, got %d", n)
	}
	if n := InboxesWithinBudget(32768, 0); n != 0 {
		t.Errorf("zero bytes-per-user should yield 0, got %d", n)
	}
}
