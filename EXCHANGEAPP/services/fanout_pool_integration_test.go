package services

import (
	"exchangeapp/global"
	"exchangeapp/models"
	"fmt"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// 这个测试存在的理由是一次真实的事故：
//
// B 类的 worker 池 handler 原本调的是 execute()，而 execute() 会重新跑一遍
// 路由 —— 判定还是 B 类，于是**把文章又投回同一个池**。worker 取出来再投回去，
// 无限弹跳：不推、不报错、日志里一片空白，只是 CPU 烧起来。
//
// 这类 bug 单看代码很容易滑过去（每一行都对，错的是一句话的语义），
// 只有「真的跑一遍、然后断言副作用发生了」才能抓到。
// 所以这个测试断言的不是返回值，而是 **feed:timeline:<fid> 里真的多了东西**。

func setupTestDB(t *testing.T) {
	t.Helper()

	dsn := "root:ljf356368@tcp(127.0.0.1:3306)/exchangeapp?charset=utf8mb4&parseTime=True&loc=Local"
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Skipf("MySQL not available, skipping integration test: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("MySQL not reachable, skipping integration test")
	}

	prev := global.Db
	global.Db = db
	t.Cleanup(func() { global.Db = prev })
}

const (
	testFollowerBase = 9991100
	testFollowers    = 10
)

// seedTierBAuthor 造一个专属作者 + N 个粉丝。
//
// 用低阈值把 10 个粉丝放大成「B 类」，而不是真造 5 万行 —— 路由是配置驱动的，
// 这里要测的是「进入 B 类分支之后会不会终止」，粉丝量的绝对值不重要。
// 作者必须是一条真实的 users 记录：articles.author_id 上有外键。
func seedTierBAuthor(t *testing.T) uint {
	t.Helper()

	// LastActiveAt 必须显式给值：零值会被写成 '0000-00-00'，MySQL 直接拒收
	author := models.User{
		Username:     "__fanout_pool_test__",
		Password:     "x",
		LastActiveAt: time.Now(),
	}
	if err := global.Db.Create(&author).Error; err != nil {
		t.Fatalf("create test author: %v", err)
	}

	followers := make([]uint, 0, testFollowers)
	for i := 0; i < testFollowers; i++ {
		fid := uint(testFollowerBase + i)
		row := models.Follow{FollowerID: fid, FollowingID: author.ID}
		if err := global.Db.Create(&row).Error; err != nil {
			t.Fatalf("seed follower %d: %v", fid, err)
		}
		followers = append(followers, fid)
	}

	t.Cleanup(func() {
		// 必须 Unscoped：Follow 带 gorm.Model 软删除，普通 Delete 只会写
		// deleted_at，而作者那行 users 是硬删的 —— 结果是每跑一次测试就留下
		// N 行指向不存在用户的孤儿 follow 记录（实测跑 5 次积了 50 行）。
		global.Db.Unscoped().Where("following_id = ?", author.ID).Delete(&models.Follow{})
		global.Db.Unscoped().Where("author_id = ?", author.ID).Delete(&models.Article{})
		global.Db.Unscoped().Delete(&models.User{}, author.ID)
		for _, fid := range followers {
			global.RedisDB.Del(fanoutCtx, fmt.Sprintf("feed:timeline:%d", fid))
		}
	})
	return author.ID
}

func TestTierBAsyncFanoutActuallyTerminates(t *testing.T) {
	setupTestRedis(t)
	setupTestDB(t)
	authorID := seedTierBAuthor(t)

	cfg := testCfg()
	// 10 个粉丝 → 落在 [5, 1000) → B 类（异步推）
	cfg.Feed.TierAMaxFollowers = 5
	cfg.Feed.TierCMinFollowers = 1000
	cfg.Feed.FanoutBatchSize = 4 // 逼出多批，顺便覆盖分页游标
	cfg.Feed.MaterializeActiveOnly = false
	cfg.Feed.FanoutRetryScanMs = 100

	svc := NewBigVFanoutService(cfg, nil, nil)
	defer svc.Stop()

	now := time.Now().Unix()
	if got := svc.PlanArticle(outboxArticle(1, authorID, now)).Strategy; got != StrategyAsyncPush {
		t.Fatalf("fixture is not in tier B (strategy=%s); the rest of this test is meaningless", got)
	}

	article := outboxArticle(999900, authorID, now)
	article.Title = "tier-B async fanout"
	article.Status = "published"
	if err := global.Db.Create(&article).Error; err != nil {
		t.Fatalf("create article: %v", err)
	}

	outcome := svc.FanoutArticle(article)
	if outcome.Err != nil {
		t.Fatalf("FanoutArticle returned error: %v", outcome.Err)
	}
	if !outcome.Deferred {
		t.Fatal("tier-B article should be deferred to the async pool")
	}

	// 断言「副作用真的发生了」——而不是只看返回值。
	// 递归投池的 bug 会让这里永远等不到结果，最终超时失败。
	deadline := time.Now().Add(5 * time.Second)
	for {
		written := 0
		for i := 0; i < testFollowers; i++ {
			fid := uint(testFollowerBase + i)
			key := fmt.Sprintf("feed:timeline:%d", fid)
			if _, err := global.RedisDB.ZScore(fanoutCtx, key, fmt.Sprint(article.ID)).Result(); err == nil {
				written++
			}
		}
		if written == testFollowers {
			return // ✓ 全部粉丝都拿到了时间线条目
		}
		if time.Now().After(deadline) {
			t.Fatalf("async fanout did not finish within 5s: %d/%d followers written. "+
				"A tier-B article that never lands in any timeline usually means the worker "+
				"is re-submitting it into its own pool instead of pushing it.",
				written, testFollowers)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 队列拥塞时必须返回 false 让调用方入重试队列，而不是静默丢弃
func TestPoolCongestionIsReportedNotSwallowed(t *testing.T) {
	release := make(chan struct{})
	pool := NewFanoutWorkerPool(1, 1, func(models.Article) {
		<-release // 占住唯一的 worker，直到测试放行
	})
	defer func() {
		close(release)
		pool.Stop()
	}()

	// worker 里 1 条 + 缓冲区 1 条 → 第 3 条开始必然拥塞
	pool.Submit(outboxArticle(1, 1, time.Now().Unix()))
	pool.Submit(outboxArticle(2, 1, time.Now().Unix()))

	if pool.Submit(outboxArticle(3, 1, time.Now().Unix())) {
		t.Fatal("Submit should report congestion instead of silently queueing unboundedly")
	}
	if pool.Submit(outboxArticle(4, 1, time.Now().Unix())) {
		t.Fatal("Submit should keep reporting congestion while the worker is blocked")
	}
}
