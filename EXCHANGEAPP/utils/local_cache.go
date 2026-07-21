package utils

import (
	"sync"
	"time"
)

// cacheEntry 缓存条目，带过期时间
type cacheEntry struct {
	value   any
	expires time.Time
}

// LocalCache 本地内存缓存，用于高频读取的小数据（如关注列表）
// 等价于 Java Caffeine，但更轻量，适合 Feed 流 PULL 模式下的热点数据
type LocalCache struct {
	mu       sync.RWMutex
	store    map[string]*cacheEntry
	maxSize  int
	stopCh   chan struct{}
}

// NewLocalCache 创建本地缓存，maxSize 限制最大条目数
func NewLocalCache(maxSize int, cleanupInterval time.Duration) *LocalCache {
	lc := &LocalCache{
		store:   make(map[string]*cacheEntry),
		maxSize: maxSize,
		stopCh:  make(chan struct{}),
	}
	go lc.cleanupLoop(cleanupInterval)
	return lc
}

// Get 读取缓存，若不存在或已过期返回 nil, false
func (lc *LocalCache) Get(key string) (any, bool) {
	lc.mu.RLock()
	entry, ok := lc.store[key]
	lc.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expires) {
		lc.mu.Lock()
		delete(lc.store, key)
		lc.mu.Unlock()
		return nil, false
	}
	return entry.value, true
}

// Set 写入缓存，带 TTL
func (lc *LocalCache) Set(key string, value any, ttl time.Duration) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	// 容量保护：超过 maxSize 时随机淘汰一半
	if len(lc.store) >= lc.maxSize {
		count := 0
		for k := range lc.store {
			if count >= lc.maxSize/2 {
				break
			}
			delete(lc.store, k)
			count++
		}
	}

	lc.store[key] = &cacheEntry{
		value:   value,
		expires: time.Now().Add(ttl),
	}
}

// Delete 删除缓存条目
func (lc *LocalCache) Delete(key string) {
	lc.mu.Lock()
	delete(lc.store, key)
	lc.mu.Unlock()
}

// InvalidatePrefix 删除所有匹配前缀的 key（用于缓存批量失效）
func (lc *LocalCache) InvalidatePrefix(prefix string) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	for k := range lc.store {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(lc.store, k)
		}
	}
}

// Size 返回当前缓存条目数
func (lc *LocalCache) Size() int {
	lc.mu.RLock()
	defer lc.mu.RUnlock()
	return len(lc.store)
}

// cleanupLoop 定期清理过期条目
func (lc *LocalCache) cleanupLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			lc.mu.Lock()
			now := time.Now()
			for k, entry := range lc.store {
				if now.After(entry.expires) {
					delete(lc.store, k)
				}
			}
			lc.mu.Unlock()
		case <-lc.stopCh:
			return
		}
	}
}

// Stop 停止清理协程
func (lc *LocalCache) Stop() {
	close(lc.stopCh)
}
