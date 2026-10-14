package utils

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

// ActiveWindow 本地滑动窗口 + Redis 异步刷新的活跃用户追踪器
//
// 两级缓存：
//
//	L1（本地 map）: dirty → 待 flush  |  clean → 已 flush 热缓存
//	L2（Redis）:    user:active:<id> 全局数据源
//
// 写入：MarkActive → 只写 dirty map（0 网络 IO）→ 定时批量 Pipeline flush 到 Redis
// 读取：IsActive → clean/dirty → Redis EXISTS（仅本地 miss 时才走网络）
type ActiveWindow struct {
	mu      sync.RWMutex
	dirty   map[uint]int64 // 待 flush: uid → timestamp
	clean   map[uint]int64 // 热缓存: uid → timestamp
	maxSize int

	client        *redis.Client
	flushInterval time.Duration
	activeTTL     time.Duration
	stopCh        chan struct{}
}

// NewActiveWindow 创建本地活跃窗口
func NewActiveWindow(client *redis.Client, flushInterval time.Duration) *ActiveWindow {
	if flushInterval <= 0 {
		flushInterval = 1 * time.Second
	}
	aw := &ActiveWindow{
		dirty:         make(map[uint]int64),
		clean:         make(map[uint]int64),
		maxSize:       50000,
		client:        client,
		flushInterval: flushInterval,
		activeTTL:     7 * 24 * time.Hour,
		stopCh:        make(chan struct{}),
	}
	go aw.flushLoop()
	return aw
}

// MarkActiveAndDetectReturn 标记活跃，并返回「这是不是离开后的第一次回来」
//
// 判定依据：**写入之前** `user:active:<id>` 在 Redis 里不存在。
//
// 这个 key 的 TTL（activeTTL = 7 天）正好就是维度二用来判断「要不要给他物化 inbox」
// 的窗口。所以「key 不存在」等价于「上一个 7 天窗口里他没出现过」，
// 也就等价于「这段时间的 fanout 跳过了他，他的 timeline 缺了这段内容」——
// 读路径必须重建，否则一个离开一周的读者回来只会看到一周前的旧内容。
//
// 这一次判定只可能为真一次：紧接着 MarkActive 就把标记写上了（1 秒后 flush 到
// Redis），之后所有请求看到的都是「已存在」。所以它是**一次性触发**，
// 正好配合读路径的重建 + MaterializeInbox 自愈——补一次就够了。
//
// 调用频率有天然节流：AuthMiddleware.refreshLastActive 用 SETNX 做了 5 分钟节流，
// 所以这个额外的 EXISTS 每个用户最多每 5 分钟一次，不是每请求一次。
func (aw *ActiveWindow) MarkActiveAndDetectReturn(userID uint) bool {
	key := fmt.Sprintf("user:active:%d", userID)
	exists, err := aw.client.Exists(context.Background(), key).Result()

	aw.MarkActive(userID) // 仍然只是写本地 map，0 网络 IO

	if err != nil {
		// Redis 故障时不触发重建：故障期间全量读者都「像刚回来」，
		// 会给 DB 放大出一波重建风暴，比少看到几条更糟
		return false
	}
	return exists == 0
}

// MarkActive 标记用户活跃（0 网络 IO，仅写本地 map）
func (aw *ActiveWindow) MarkActive(userID uint) {
	aw.mu.Lock()
	aw.dirty[userID] = time.Now().Unix()
	if len(aw.dirty) > aw.maxSize {
		for k := range aw.dirty {
			delete(aw.dirty, k)
			if len(aw.dirty) <= aw.maxSize/2 {
				break
			}
		}
	}
	aw.mu.Unlock()
}

// isFresh 判断本地缓存的活跃时间戳是否还在 TTL 内。
//
// 修复的 bug：clean/dirty 两个 map 都存了时间戳（uid → unix 秒），
// 但 IsActive/FilterInactive 只检查 key 是否存在，从不看时间戳。
// 结果是「7 天活跃」退化成「曾经活跃过」—— 用户访问过一次就被永久判为活跃，
// 只有重启服务才能复位。
//
// 这不只是语义问题：维度二（只为活跃读者物化 inbox，§4.3）直接依赖这个判断，
// L1 不失效的话「非活跃读者」永远是空集，整个维度失效。
func (aw *ActiveWindow) isFresh(ts int64) bool {
	return time.Since(time.Unix(ts, 0)) < aw.activeTTL
}

// IsActive 检查用户是否活跃（L1 本地 → L2 Redis）
func (aw *ActiveWindow) IsActive(userID uint) bool {
	aw.mu.RLock()
	cleanTS, inClean := aw.clean[userID]
	dirtyTS, inDirty := aw.dirty[userID]
	aw.mu.RUnlock()

	// L1 命中且未过期才算活跃
	if (inClean && aw.isFresh(cleanTS)) || (inDirty && aw.isFresh(dirtyTS)) {
		return true
	}

	// L1 过期 → 清掉，别让它一直挡着后面的 Redis 查询
	if inClean || inDirty {
		aw.mu.Lock()
		delete(aw.clean, userID)
		delete(aw.dirty, userID)
		aw.mu.Unlock()
	}

	key := fmt.Sprintf("user:active:%d", userID)
	exists, err := aw.client.Exists(context.Background(), key).Result()
	if err != nil {
		return true // 降级：Redis 故障时宁可多推，不可漏推
	}
	return exists > 0
}

// FilterInactive 批量过滤不活跃用户（L1 → Redis Pipeline）
// 返回的是**不活跃**的那部分；调用方需要用「入参减去返回值」得到活跃集合。
func (aw *ActiveWindow) FilterInactive(userIDs []uint) []uint {
	if len(userIDs) == 0 {
		return nil
	}

	aw.mu.RLock()
	var redisMiss []uint
	var stale []uint
	for _, uid := range userIDs {
		cleanTS, inClean := aw.clean[uid]
		dirtyTS, inDirty := aw.dirty[uid]

		// L1 命中且未过期 → 确定活跃，不用问 Redis
		if (inClean && aw.isFresh(cleanTS)) || (inDirty && aw.isFresh(dirtyTS)) {
			continue
		}
		if inClean || inDirty {
			stale = append(stale, uid)
		}
		redisMiss = append(redisMiss, uid)
	}
	aw.mu.RUnlock()

	// 过期的 L1 条目要清掉，否则它们会一直被视为缓存命中
	if len(stale) > 0 {
		aw.mu.Lock()
		for _, uid := range stale {
			delete(aw.clean, uid)
			delete(aw.dirty, uid)
		}
		aw.mu.Unlock()
	}

	if len(redisMiss) == 0 {
		return nil // 全部命中本地 → 全部活跃
	}

	// Pipeline 批量查 Redis
	pipe := aw.client.Pipeline()
	cmds := make([]*redis.IntCmd, len(redisMiss))
	for i, uid := range redisMiss {
		key := fmt.Sprintf("user:active:%d", uid)
		cmds[i] = pipe.Exists(context.Background(), key)
	}
	pipe.Exec(context.Background())

	var inactive []uint
	for i, cmd := range cmds {
		if cmd.Val() == 0 {
			inactive = append(inactive, redisMiss[i])
		}
	}
	return inactive
}

func (aw *ActiveWindow) flushLoop() {
	ticker := time.NewTicker(aw.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			aw.flush()
		case <-aw.stopCh:
			aw.flush()
			return
		}
	}
}

func (aw *ActiveWindow) flush() {
	aw.mu.Lock()
	if len(aw.dirty) == 0 {
		aw.mu.Unlock()
		return
	}
	toFlush := aw.dirty
	aw.dirty = make(map[uint]int64, len(toFlush))
	aw.mu.Unlock()

	pipe := aw.client.Pipeline()
	for uid := range toFlush {
		key := fmt.Sprintf("user:active:%d", uid)
		pipe.Set(context.Background(), key, "1", aw.activeTTL)
	}
	if _, err := pipe.Exec(context.Background()); err != nil {
		return
	}

	aw.mu.Lock()
	for uid, ts := range toFlush {
		aw.clean[uid] = ts
	}
	if len(aw.clean) > aw.maxSize {
		for k := range aw.clean {
			delete(aw.clean, k)
			if len(aw.clean) <= aw.maxSize/2 {
				break
			}
		}
	}
	aw.mu.Unlock()
}

func (aw *ActiveWindow) Stop() {
	close(aw.stopCh)
}

func (aw *ActiveWindow) Stats() map[string]any {
	aw.mu.RLock()
	defer aw.mu.RUnlock()
	return map[string]any{
		"dirty_count": len(aw.dirty),
		"clean_count": len(aw.clean),
	}
}
