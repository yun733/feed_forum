package utils

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
)

func setupActiveWindow(t *testing.T) (*ActiveWindow, *redis.Client) {
	t.Helper()

	client := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: 15})
	if err := client.Ping(context.Background()).Err(); err != nil {
		client.Close()
		t.Skipf("Redis not available, skipping: %v", err)
	}
	client.FlushDB(context.Background())

	// 直接构造，不启动 flushLoop —— 测试自己控制 flush 时机，避免定时器和断言竞争
	aw := &ActiveWindow{
		dirty:         make(map[uint]int64),
		clean:         make(map[uint]int64),
		maxSize:       1000,
		client:        client,
		flushInterval: time.Hour,
		activeTTL:     7 * 24 * time.Hour,
		stopCh:        make(chan struct{}),
	}

	t.Cleanup(func() {
		client.FlushDB(context.Background())
		client.Close()
	})
	return aw, client
}

func TestIsActiveSeesDirtyEntry(t *testing.T) {
	aw, _ := setupActiveWindow(t)

	aw.MarkActive(1)
	if !aw.IsActive(1) {
		t.Error("just-marked user should be active")
	}
}

// 回归测试：L1 本地缓存原本没有 TTL 校验 —— IsActive/FilterInactive 只检查
// key 是否存在，从不看存进去的时间戳。结果是「7 天活跃」退化成「曾经活跃过」，
// 用户访问过一次就被永久判为活跃，只有重启服务才能复位。
//
// 这个 bug 不只是语义问题：维度二（只为活跃读者物化 inbox）直接依赖这个判断，
// L1 不失效的话「非活跃读者」永远是空集，整个维度失效。
func TestStaleL1EntryDoesNotCountAsActive(t *testing.T) {
	aw, _ := setupActiveWindow(t)

	// 模拟「很久以前访问过一次，之后 L1 里的条目一直没被清掉，
	// Redis 侧的 key 也已经过期」
	aw.mu.Lock()
	aw.clean[42] = time.Now().Add(-8 * 24 * time.Hour).Unix()
	aw.mu.Unlock()

	if aw.IsActive(42) {
		t.Fatal("stale L1 entry (8 days old, beyond the 7-day window) must not count as active")
	}

	// 顺手断言过期条目被清掉了，否则它会一直挡着后面的 Redis 查询
	aw.mu.RLock()
	_, stillThere := aw.clean[42]
	aw.mu.RUnlock()
	if stillThere {
		t.Error("stale L1 entry should have been evicted on read")
	}
}

func TestFreshL1EntryStillShortCircuits(t *testing.T) {
	aw, client := setupActiveWindow(t)

	// L1 里有新鲜条目时不该去问 Redis：这是 ActiveWindow 存在的意义（0 网络 IO）
	aw.mu.Lock()
	aw.clean[7] = time.Now().Unix()
	aw.mu.Unlock()

	client.Close() // 关掉 Redis，L1 命中就不该受影响
	if !aw.IsActive(7) {
		t.Error("fresh L1 entry should short-circuit without touching Redis")
	}
}

func TestFilterInactiveIgnoresStaleEntries(t *testing.T) {
	aw, _ := setupActiveWindow(t)

	aw.mu.Lock()
	aw.clean[1] = time.Now().Unix()                          // 新鲜 → 活跃
	aw.clean[2] = time.Now().Add(-9 * 24 * time.Hour).Unix() // 过期 → 不算活跃
	aw.mu.Unlock()

	inactive := aw.FilterInactive([]uint{1, 2, 3})

	// 1 新鲜（活跃），2 过期（不活跃），3 从没出现过（不活跃）
	want := map[uint]bool{2: true, 3: true}
	if len(inactive) != len(want) {
		t.Fatalf("expected inactive %v, got %v", want, inactive)
	}
	for _, id := range inactive {
		if !want[id] {
			t.Errorf("user %d should not be reported as inactive", id)
		}
	}
}

// flush 必须把时间戳写进 Redis，而不是恒为 "1"：
// 读路径靠这个 key 的「存在与否」判断用户是不是刚从 7 天窗口外回来
// （MarkActiveAndDetectReturn），值本身不影响判断，但时间戳让排查有依据。
func TestFlushWritesActiveKeyWithTTL(t *testing.T) {
	aw, client := setupActiveWindow(t)

	aw.MarkActive(5)
	aw.flush()

	key := fmt.Sprintf("user:active:%d", 5)
	if n := client.Exists(context.Background(), key).Val(); n != 1 {
		t.Fatalf("flush should have written %s", key)
	}
	ttl := client.TTL(context.Background(), key).Val()
	if ttl <= 0 || ttl > aw.activeTTL {
		t.Errorf("active key TTL = %v, want (0, %v]", ttl, aw.activeTTL)
	}
}

// 这是维度二的判定入口：key 不存在 ⟺ 上一个 7 天窗口里没出现过
// ⟺ 期间 fanout 跳过了他 ⟺ 读路径必须强制重建 timeline
func TestMarkActiveAndDetectReturn(t *testing.T) {
	aw, client := setupActiveWindow(t)

	// 从未出现过 → 第一次访问算「刚回来」（他的 timeline 是空的，需要重建）
	if !aw.MarkActiveAndDetectReturn(9) {
		t.Error("user with no active key should be treated as returning")
	}

	aw.flush() // 把 9 写进 Redis

	// 紧接着的访问：虽然 L1 里有新鲜条目，但 Redis key 已经存在 → 不算刚回来
	if aw.MarkActiveAndDetectReturn(9) {
		t.Error("user whose active key exists should not be treated as returning")
	}

	// 模拟离开 7 天以上：Redis 的 key 过期了
	client.Del(context.Background(), "user:active:9")
	aw.mu.Lock()
	delete(aw.clean, 9)
	aw.mu.Unlock()

	if !aw.MarkActiveAndDetectReturn(9) {
		t.Error("user returning after the active key expired must be treated as returning")
	}
}
