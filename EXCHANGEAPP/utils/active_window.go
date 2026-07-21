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
//   L1（本地 map）: dirty → 待 flush  |  clean → 已 flush 热缓存
//   L2（Redis）:    user:active:<id> 全局数据源
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

// IsActive 检查用户是否活跃（L1 本地 → L2 Redis）
func (aw *ActiveWindow) IsActive(userID uint) bool {
	aw.mu.RLock()
	_, inClean := aw.clean[userID]
	_, inDirty := aw.dirty[userID]
	aw.mu.RUnlock()
	if inClean || inDirty {
		return true
	}

	key := fmt.Sprintf("user:active:%d", userID)
	exists, err := aw.client.Exists(context.Background(), key).Result()
	if err != nil {
		return true // 降级
	}
	return exists > 0
}

// FilterInactive 批量过滤不活跃用户（L1 → Redis Pipeline）
func (aw *ActiveWindow) FilterInactive(userIDs []uint) []uint {
	if len(userIDs) == 0 {
		return nil
	}

	aw.mu.RLock()
	var redisMiss []uint
	var redisMissIdx []int
	for i, uid := range userIDs {
		_, inClean := aw.clean[uid]
		_, inDirty := aw.dirty[uid]
		if !inClean && !inDirty {
			redisMiss = append(redisMiss, uid)
			redisMissIdx = append(redisMissIdx, i)
		}
	}
	aw.mu.RUnlock()

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
