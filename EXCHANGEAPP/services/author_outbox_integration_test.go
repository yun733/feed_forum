package services

import (
	"exchangeapp/config"
	"exchangeapp/global"
	"exchangeapp/models"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
)

// 这些测试打真实的 Redis（用 DB 15，避开业务库 0），Redis 不可用就跳过。
// 覆盖的是新增的两个数据结构：
//
//	author:outbox:<uid>  —— C 类作者的 outbox
//	feed:authors:c       —— 当前 C 类作者集合
//
// 它们是「大V走拉」的地基，写错一个 score 或 member 就会静默丢文章。
func setupTestRedis(t *testing.T) {
	t.Helper()

	client := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: 15})
	if err := client.Ping(fanoutCtx).Err(); err != nil {
		client.Close()
		t.Skipf("Redis not available, skipping integration test: %v", err)
	}

	prev := global.RedisDB
	global.RedisDB = client
	client.FlushDB(fanoutCtx)

	t.Cleanup(func() {
		client.FlushDB(fanoutCtx)
		client.Close()
		global.RedisDB = prev
	})
}

func testCfg() config.Config {
	var cfg config.Config
	cfg.Feed.TierAMaxFollowers = 5000
	cfg.Feed.TierCMinFollowers = 500000
	cfg.Feed.OutboxTTLDays = 30
	cfg.Feed.OutboxMaxItems = 500
	cfg.Feed.OutboxFetchLimit = 50
	cfg.Feed.MaxTierCAuthorsPerUser = 100
	cfg.Feed.TierCSetCacheTTLSeconds = 60
	return cfg
}

func outboxArticle(id, authorID uint, ts int64) models.Article {
	a := models.Article{}
	a.ID = id
	a.AuthorID = authorID
	a.CreatedAt = time.Unix(ts, 0)
	return a
}

func TestOutboxAppendAndFetchOrder(t *testing.T) {
	setupTestRedis(t)
	svc := NewAuthorOutboxService(testCfg())

	now := time.Now().Unix()
	// 刻意乱序写入：outbox 必须按 score（发文时间）而不是写入顺序排序
	if err := svc.Append(outboxArticle(1, 7, now-300)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := svc.Append(outboxArticle(3, 7, now-100)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := svc.Append(outboxArticle(2, 7, now-200)); err != nil {
		t.Fatalf("append: %v", err)
	}

	got := svc.FetchRecent([]uint{7}, 0)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(got), got)
	}

	want := []uint{3, 2, 1} // 时间倒序
	for i, w := range want {
		if got[i].ArticleID != w {
			t.Errorf("position %d: got article %d, want %d", i, got[i].ArticleID, w)
		}
	}
}

// outbox 是「每个 C 类作者一个 key」，所以 K 路归并要能把多个作者的
// 内容按时间交错合并 —— 这是大V走拉的核心读操作。
func TestOutboxMergeAcrossAuthors(t *testing.T) {
	setupTestRedis(t)
	svc := NewAuthorOutboxService(testCfg())

	now := time.Now().Unix()
	// 作者 7 和作者 9 交替发文
	svc.Append(outboxArticle(1, 7, now-500))
	svc.Append(outboxArticle(2, 9, now-400))
	svc.Append(outboxArticle(3, 7, now-300))
	svc.Append(outboxArticle(4, 9, now-200))
	svc.Append(outboxArticle(5, 7, now-100))

	got := svc.FetchRecent([]uint{7, 9}, 0)
	if len(got) != 5 {
		t.Fatalf("expected 5 merged entries, got %d", len(got))
	}

	want := []uint{5, 4, 3, 2, 1}
	for i, w := range want {
		if got[i].ArticleID != w {
			t.Errorf("position %d: got article %d, want %d (merge must interleave by time)",
				i, got[i].ArticleID, w)
		}
	}
}

// 头部缓存必须能被 Append 精确失效：否则大V发完文章，
// 读者在 TTL 内看不到（而 TTL 过期的那一刻正是读尖峰）
func TestOutboxHeadCacheInvalidatedByAppend(t *testing.T) {
	setupTestRedis(t)
	svc := NewAuthorOutboxService(testCfg())

	now := time.Now().Unix()
	svc.Append(outboxArticle(1, 7, now-100))

	if got := svc.FetchRecent([]uint{7}, 0); len(got) != 1 {
		t.Fatalf("expected 1 entry after first append, got %d", len(got))
	}
	// 再读一次：这一次应该命中本地缓存
	if got := svc.FetchRecent([]uint{7}, 0); len(got) != 1 {
		t.Fatalf("expected cached 1 entry, got %d", len(got))
	}

	// 新文章进来 → 缓存必须立刻失效，不能等 TTL
	if err := svc.Append(outboxArticle(2, 7, now)); err != nil {
		t.Fatalf("append: %v", err)
	}
	got := svc.FetchRecent([]uint{7}, 0)
	if len(got) != 2 {
		t.Fatalf("head cache was not invalidated by Append: got %d entries, want 2", len(got))
	}
	if got[0].ArticleID != 2 {
		t.Errorf("newest article should be first, got %d", got[0].ArticleID)
	}
}

// 缓存粒度是「按作者」而不是「按用户关注的大V集合」：
// 一个大V发文不能把其他大V的头部缓存一起清掉，
// 因为清缓存的那一刻正是所有读者的读尖峰打进来的时候。
func TestOutboxHeadCacheIsPerAuthor(t *testing.T) {
	setupTestRedis(t)
	svc := NewAuthorOutboxService(testCfg())

	now := time.Now().Unix()
	svc.Append(outboxArticle(1, 7, now-100))
	svc.Append(outboxArticle(2, 9, now-100))

	svc.FetchRecent([]uint{7, 9}, 0) // 把两个作者的头部都灌进缓存

	// 只有作者 7 发了新文章
	svc.Append(outboxArticle(3, 7, now))

	if _, ok := svc.cachedHead(9); !ok {
		t.Error("author 9's head cache should survive an append by author 7")
	}
}

func TestOutboxCursorPagination(t *testing.T) {
	setupTestRedis(t)
	svc := NewAuthorOutboxService(testCfg())

	now := time.Now().Unix()
	for i := uint(1); i <= 5; i++ {
		svc.Append(outboxArticle(i, 7, now-int64(10*i)))
	}

	// 游标语义必须和 timeline 一致：max = cursor.Score - 1
	all := svc.FetchRecent([]uint{7}, 0)
	if len(all) != 5 {
		t.Fatalf("expected 5, got %d", len(all))
	}

	page2 := svc.FetchRecent([]uint{7}, all[1].Score)
	if len(page2) != 3 {
		t.Fatalf("expected 3 entries strictly older than the cursor, got %d: %+v", len(page2), page2)
	}
	for _, e := range page2 {
		if e.Score >= all[1].Score {
			t.Errorf("entry score %d must be strictly below cursor %d", e.Score, all[1].Score)
		}
	}
}

func TestTierCRegistrationAndFiltering(t *testing.T) {
	setupTestRedis(t)
	svc := NewAuthorOutboxService(testCfg())

	// 未登记时短路：读路径完全不去查关注列表
	if svc.HasTierCAuthors() {
		t.Fatal("no tier-C authors registered yet")
	}

	if err := svc.RegisterTierC(7); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := svc.RegisterTierC(9); err != nil {
		t.Fatalf("register: %v", err)
	}

	// 用户同时关注 3（小作者）、7、9（大V）：只有 7 和 9 该被归并
	got := svc.FollowedTierCAuthors([]uint{3, 7, 9})
	if len(got) != 2 {
		t.Fatalf("expected 2 tier-C authors, got %v", got)
	}
	for _, id := range got {
		if id != 7 && id != 9 {
			t.Errorf("author %d should not be treated as tier-C", id)
		}
	}
	if !svc.IsTierC(7) {
		t.Error("IsTierC(7) should be true after Register")
	}

	// 掉粉退出 C 类后，读路径不该再归并他的 outbox
	if err := svc.UnregisterTierC(7); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if svc.IsTierC(7) {
		t.Error("IsTierC(7) should be false after Unregister")
	}
}

// K 的硬上限（§8）：读一次 Feed 的归并代价随 K 线性增长，必须封顶
func TestTierCAuthorsCappedAtK(t *testing.T) {
	setupTestRedis(t)

	cfg := testCfg()
	cfg.Feed.MaxTierCAuthorsPerUser = 3
	svc := NewAuthorOutboxService(cfg)

	following := make([]uint, 0, 10)
	for id := uint(1); id <= 10; id++ {
		svc.RegisterTierC(id)
		following = append(following, id)
	}

	if got := svc.FollowedTierCAuthors(following); len(got) != 3 {
		t.Errorf("expected K=3 tier-C authors, got %d", len(got))
	}
}

// 重试队列的 attempt 记账：Enqueue 写进去的 member 决定下一次读出来的 attempt，
// 而 worker 靠 member 精确删除。两者算错一步就会「重试不推进」或「文章被误删」。
func TestRetryQueueAttemptBookkeeping(t *testing.T) {
	setupTestRedis(t)

	q := NewFanoutRetryQueue(3)

	// 首次入队（attempt=0）→ member "42:1"，表示「第 1 次重试将会尝试」
	q.Enqueue(42, 0, "boom")

	// 指数退避把首次重试排到 2s 之后（见 TestRetryQueueBackoffIsDelayed），
	// 测试里把 score 拉到 0 来模拟「时间到了」
	makeDue := func(member string) {
		global.RedisDB.ZAdd(fanoutCtx, fanoutRetryKey, &redis.Z{Score: 0, Member: member})
	}
	makeDue("42:1")

	due := q.Due(10)
	if len(due) != 1 {
		t.Fatalf("expected 1 due item, got %d", len(due))
	}
	if due[0].ArticleID != 42 || due[0].Attempt != 1 {
		t.Fatalf("expected article 42 attempt 1, got %+v", due[0])
	}

	// worker 的记账顺序：先按 member 把取出来的那条删掉，再按 attempt+1 重新入队
	q.Complete(due[0])
	q.Enqueue(due[0].ArticleID, due[0].Attempt, "boom again")
	makeDue("42:2")

	due2 := q.Due(10)
	if len(due2) != 1 || due2[0].Attempt != 2 {
		t.Fatalf("expected attempt to advance to 2, got %+v", due2)
	}

	// 超过上限后不再入队，只告警
	q.Complete(due2[0])
	q.Enqueue(due2[0].ArticleID, 3, "still failing")

	if got := q.Depth(); got != 0 {
		t.Errorf("exhausted article should be dropped from the queue, depth=%d", got)
	}
}

// 退避必须是递增的：连续重试不能同刻一起打回来
func TestRetryQueueBackoffIsDelayed(t *testing.T) {
	setupTestRedis(t)

	q := NewFanoutRetryQueue(5)
	q.Enqueue(1, 0, "boom")

	// 首次重试要等 2s，所以此刻不该到点
	if got := q.Due(10); len(got) != 0 {
		t.Errorf("item should not be due immediately, got %+v", got)
	}
	if q.Depth() != 1 {
		t.Errorf("item should still be queued, depth=%d", q.Depth())
	}
}
