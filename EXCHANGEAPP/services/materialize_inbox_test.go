package services

import (
	"exchangeapp/global"
	"exchangeapp/models"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
)

// 这组测试保护的是「非活跃读者回来时，补的内容真的留下来了」这件事。
//
// 之前的实现只把重建结果写进 feed:pull:cache（30s），而那份缓存只有 need>0
// 时才会被读到 —— 读者回来时页面是满的，need 本来是 0，是 feedReturning
// 把它顶起来的。于是补一次内容、下一次请求 need 又变回 0、缓存再也不会被读到，
// 用户刷新一下就退回旧内容。MaterializeInbox 就是把这个缺口补上的。

func newMaterializeService(t *testing.T, maxItems, ttlDays int) *BigVFanoutService {
	t.Helper()
	cfg := testCfg()
	cfg.Feed.TimelineMaxItems = maxItems
	cfg.Feed.TimelineTTLDays = ttlDays
	svc := NewBigVFanoutService(cfg, nil, nil)
	t.Cleanup(svc.Stop)
	return svc
}

func TestMaterializeInboxWritesAndKeepsNewest(t *testing.T) {
	setupTestRedis(t)
	svc := newMaterializeService(t, 5, 30)

	now := time.Now().Unix()
	// 写 8 条，maxItems=5 → 应该只留下最新的 5 条
	var articles []models.Article
	for i := 1; i <= 8; i++ {
		articles = append(articles, outboxArticle(uint(i), 1, now-int64(i*10)))
	}

	n, err := svc.MaterializeInbox(42, articles)
	if err != nil {
		t.Fatalf("MaterializeInbox: %v", err)
	}
	if n != 8 {
		t.Errorf("expected 8 articles written, got %d", n)
	}

	got, err := global.RedisDB.ZRevRange(fanoutCtx, "feed:timeline:42", 0, -1).Result()
	if err != nil {
		t.Fatalf("zrevrange: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("timeline must be trimmed to maxItems=5, got %d: %v", len(got), got)
	}

	// 留下的必须是最新的 5 条：文章 1..5（时间倒序），砍掉的是最旧的 6/7/8
	want := []string{"1", "2", "3", "4", "5"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("position %d: got article %s, want %s (newest must survive the trim)", i, got[i], w)
		}
	}
}

// score 约定必须和 fanout 完全一致，否则两条路径写进去的数据
// 在同一个时间序里会排错位置 —— 读者会看到"补进来的内容插在错误的时间点"。
func TestMaterializeInboxScoresMatchFanoutConvention(t *testing.T) {
	setupTestRedis(t)
	svc := newMaterializeService(t, 500, 30)

	now := time.Now().Unix()

	// 模拟 fanout 写进来的一条（pushToTimelines 的写法：score = CreatedAt.Unix()）
	fanoutWritten := outboxArticle(100, 7, now-100)
	global.RedisDB.ZAdd(fanoutCtx, "feed:timeline:5", &redis.Z{
		Score:  float64(fanoutWritten.CreatedAt.Unix()),
		Member: "100",
	})

	// 重建补进来的三条，时间分别夹在 fanout 那条的前后
	rebuilt := []models.Article{
		outboxArticle(200, 7, now-200), // 更旧
		outboxArticle(300, 7, now-50),  // 更新
		outboxArticle(400, 7, now-150), // 中间
	}
	if _, err := svc.MaterializeInbox(5, rebuilt); err != nil {
		t.Fatalf("MaterializeInbox: %v", err)
	}

	got, _ := global.RedisDB.ZRevRange(fanoutCtx, "feed:timeline:5", 0, -1).Result()
	want := []string{"300", "100", "400", "200"} // 严格按时间倒序交错
	if len(got) != len(want) {
		t.Fatalf("expected %d entries, got %v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %s, want %s — materialized entries must interleave "+
				"with fanout-written ones by time", i, got[i], want[i])
		}
	}
}

func TestMaterializeInboxSetsTTL(t *testing.T) {
	setupTestRedis(t)
	svc := newMaterializeService(t, 500, 30)

	if _, err := svc.MaterializeInbox(9, []models.Article{outboxArticle(1, 1, time.Now().Unix())}); err != nil {
		t.Fatalf("MaterializeInbox: %v", err)
	}

	ttl, err := global.RedisDB.TTL(fanoutCtx, "feed:timeline:9").Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	want := 30 * 24 * time.Hour
	if ttl <= 0 || ttl > want {
		t.Errorf("timeline TTL = %v, want (0, %v]", ttl, want)
	}
}

// 边界：空输入和零值 ID 都不该产生写入
// （尤其不该创建空 key —— 空 timeline 会让下一次读算出 need=size，反复触发重建）
func TestMaterializeInboxNoOpOnEmpty(t *testing.T) {
	setupTestRedis(t)
	svc := newMaterializeService(t, 500, 30)

	if n, err := svc.MaterializeInbox(11, nil); n != 0 || err != nil {
		t.Errorf("nil input: got n=%d err=%v, want 0/nil", n, err)
	}
	var zeroID models.Article
	if n, err := svc.MaterializeInbox(11, []models.Article{zeroID}); n != 0 || err != nil {
		t.Errorf("zero-ID input: got n=%d err=%v, want 0/nil", n, err)
	}
	if exists, _ := global.RedisDB.Exists(fanoutCtx, "feed:timeline:11").Result(); exists != 0 {
		t.Error("no-op materialize must not create the timeline key")
	}
}
