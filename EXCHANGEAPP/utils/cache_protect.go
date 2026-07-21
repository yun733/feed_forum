package utils

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"math/big"
	"time"

	"github.com/go-redis/redis/v8"
)

// CacheProtector 缓存防护：解决击穿/穿透/雪崩三大问题
type CacheProtector struct {
	client *redis.Client
}

func NewCacheProtector(client *redis.Client) *CacheProtector {
	return &CacheProtector{client: client}
}

// ==================== 1. 缓存击穿：互斥锁 ====================
// 热点 key 过期瞬间，大量并发请求涌入 DB。
// 方案：第一个请求用 SETNX 抢锁去查 DB 并回写缓存；其余请求自旋等待或返回降级数据。

const lockPrefix = "cache:lock:"

func (p *CacheProtector) AcquireLock(ctx context.Context, key string, ttl time.Duration) bool {
	ok, _ := p.client.SetNX(ctx, lockPrefix+key, 1, ttl).Result()
	return ok
}

func (p *CacheProtector) ReleaseLock(ctx context.Context, key string) {
	p.client.Del(ctx, lockPrefix+key)
}

// GetOrSetWithLock 带互斥锁的缓存读取
// 缓存命中 → 直接返回
// 缓存未命中 → 抢锁 → 抢到查 DB 回写 → 未抢到自旋等缓存（最多等 waitTimeout）
func (p *CacheProtector) GetOrSetWithLock(
	ctx context.Context,
	cacheKey string,
	lockTTL time.Duration,
	waitTimeout time.Duration,
	fetcher func() (interface{}, error),
	cacheTTL time.Duration,
) ([]byte, error) {
	// 1. 先查缓存
	cached, err := p.client.Get(ctx, cacheKey).Bytes()
	if err == nil {
		return cached, nil
	}
	if err != redis.Nil {
		return nil, err
	}

	// 2. 缓存未命中，抢锁
	start := time.Now()
	for {
		if p.AcquireLock(ctx, cacheKey, lockTTL) {
			defer p.ReleaseLock(ctx, cacheKey)

			// Double-check：抢到锁后再次查缓存（可能其他线程已回写）
			cached, err := p.client.Get(ctx, cacheKey).Bytes()
			if err == nil {
				return cached, nil
			}

			// 查 DB
			data, err := fetcher()
			if err != nil {
				return nil, err
			}

			// 回写缓存 + 随机抖动防雪崩
			ttlWithJitter := addJitter(cacheTTL, 0.15)
			payload, _ := json.Marshal(data)
			p.client.Set(ctx, cacheKey, payload, ttlWithJitter)

			return payload, nil
		}

		// 没抢到锁，等一小会儿重试读缓存
		if time.Since(start) > waitTimeout {
			// 超时降级：直接走 DB（兜底）
			data, err := fetcher()
			if err != nil {
				return nil, err
			}
			payload, _ := json.Marshal(data)
			return payload, nil
		}

		time.Sleep(50 * time.Millisecond)

		// 再读一次缓存
		cached, err := p.client.Get(ctx, cacheKey).Bytes()
		if err == nil {
			return cached, nil
		}
	}
}

// ==================== 2. 缓存穿透：空值缓存 ====================
// 查询不存在的数据，每次穿透到 DB。
// 方案：对不存在的 key 也缓存一个特殊标记（null marker），短期有效。

const nullMarker = "__CACHE_NULL__"

func (p *CacheProtector) GetOrSetWithNullGuard(
	ctx context.Context,
	cacheKey string,
	nullTTL time.Duration,
	fetcher func() (interface{}, error),
	cacheTTL time.Duration,
) ([]byte, bool, error) {
	// isNull = true 表示数据确定不存在
	cached, err := p.client.Get(ctx, cacheKey).Bytes()
	if err == nil {
		if string(cached) == nullMarker {
			return nil, true, nil // 命中空值标记
		}
		return cached, false, nil
	}

	data, err := fetcher()
	if err != nil {
		return nil, false, err
	}

	if data == nil {
		// 缓存空值标记，短期有效，防穿透
		p.client.Set(ctx, cacheKey, nullMarker, nullTTL)
		return nil, true, nil
	}

	payload, _ := json.Marshal(data)
	ttlWithJitter := addJitter(cacheTTL, 0.15)
	p.client.Set(ctx, cacheKey, payload, ttlWithJitter)
	return payload, false, nil
}

// ==================== 3. 缓存雪崩：TTL 随机抖动 ====================
// 大量 key 同时过期，请求全部打到 DB。
// 方案：在 base TTL 上叠加 ±ratio 的随机偏移。

func addJitter(base time.Duration, ratio float64) time.Duration {
	if ratio <= 0 {
		return base
	}
	delta := int64(float64(base) * ratio)
	n, _ := rand.Int(rand.Reader, big.NewInt(delta*2+1))
	jitter := time.Duration(n.Int64() - delta)
	return base + jitter
}

// ==================== 4. 热点 Key 永不过期 + 异步刷新 ====================
// 对极端热点 key（如首页文章列表），设置逻辑过期时间而非物理过期。
// 物理 ttl 设为 -1（永不过期），value 里存 logicExpire。
// 读的时候检查逻辑时间 → 过期了先返回旧值，再异步起一个协程刷新。

type CacheValue struct {
	Data        interface{} `json:"data"`
	LogicExpire int64       `json:"logic_expire"` // Unix 毫秒
}

func (p *CacheProtector) GetOrSetLogicalExpire(
	ctx context.Context,
	cacheKey string,
	logicTTL time.Duration,
	lockTTL time.Duration,
	fetcher func() (interface{}, error),
) ([]byte, error) {
	now := time.Now().UnixMilli()

	cached, err := p.client.Get(ctx, cacheKey).Bytes()
	if err == nil {
		var cv CacheValue
		if json.Unmarshal(cached, &cv) == nil {
			if cv.LogicExpire > now {
				// 未逻辑过期，直接返回
				data, _ := json.Marshal(cv.Data)
				return data, nil
			}
			// 逻辑已过期，先返回旧值，异步刷新
			if p.AcquireLock(ctx, cacheKey, lockTTL) {
				go func() {
					defer p.ReleaseLock(context.Background(), cacheKey)
					newData, err := fetcher()
					if err != nil {
						return
					}
					cv := CacheValue{
						Data:        newData,
						LogicExpire: time.Now().Add(logicTTL).UnixMilli(),
					}
					payload, _ := json.Marshal(cv)
					p.client.Set(context.Background(), cacheKey, payload, 0) // 物理永不过期
				}()
			}
			data, _ := json.Marshal(cv.Data)
			return data, nil
		}
	}

	// 首次加载
	data, err := fetcher()
	if err != nil {
		return nil, err
	}
	cv := CacheValue{
		Data:        data,
		LogicExpire: time.Now().Add(logicTTL).UnixMilli(),
	}
	payload, _ := json.Marshal(cv)
	p.client.Set(ctx, cacheKey, payload, 0)
	return payload, nil
}
