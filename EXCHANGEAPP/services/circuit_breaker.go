package services

import (
	"context"
	"log"
	"sync"
	"time"
)

// CircuitState 熔断器状态
type CircuitState int

const (
	StateClosed   CircuitState = iota // 正常，请求放行
	StateOpen                         // 熔断，直接拒绝
	StateHalfOpen                     // 半开，尝试探测恢复
)

func (s CircuitState) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF_OPEN"
	default:
		return "UNKNOWN"
	}
}

// FanoutCircuitBreaker Feed 推送熔断器
// 当 Redis 写入延迟超阈值 → 熔断，改为全 Pull 模式
// 自动恢复：熔断后等待 cooldown，进入半开探测，成功则恢复
type FanoutCircuitBreaker struct {
	mu sync.RWMutex

	state      CircuitState
	failureCnt int
	successCnt int
	lastFailAt time.Time

	// 配置
	failureThreshold int           // 连续失败 N 次后熔断（默认 3）
	successThreshold int           // 半开状态下连续成功 N 次后恢复（默认 2）
	halfOpenMaxReqs  int           // 半开状态允许的最大探测请求数
	cooldown         time.Duration // 熔断冷却时间（默认 30s）
	timeout          time.Duration // Redis 操作超时阈值，由 feed.fanout_timeout_ms 注入

	// 统计
	totalTrips     int64  // 总熔断次数
	lastTripReason string // 最近一次熔断原因
}

// NewFanoutCircuitBreaker 创建推送熔断器
func NewFanoutCircuitBreaker(timeout time.Duration) *FanoutCircuitBreaker {
	return &FanoutCircuitBreaker{
		state:            StateClosed,
		failureThreshold: 3,
		successThreshold: 2,
		halfOpenMaxReqs:  1,
		cooldown:         30 * time.Second,
		timeout:          timeout,
	}
}

// Allow 判断当前是否允许执行推送
// 返回 true 表示放行，false 表示应降级到 Pull 模式
func (cb *FanoutCircuitBreaker) Allow() bool {
	cb.mu.RLock()
	state := cb.state
	cb.mu.RUnlock()

	switch state {
	case StateClosed:
		return true
	case StateOpen:
		// 检查是否到冷却时间了
		cb.mu.RLock()
		elapsed := time.Since(cb.lastFailAt)
		cb.mu.RUnlock()
		if elapsed >= cb.cooldown {
			cb.mu.Lock()
			if cb.state == StateOpen {
				cb.state = StateHalfOpen
				cb.successCnt = 0
				log.Printf("[CircuitBreaker] State transition: OPEN → HALF_OPEN (cooldown elapsed: %v)", elapsed)
			}
			cb.mu.Unlock()
			// 半开状态下只允许有限请求通过
			cb.mu.RLock()
			halfOpenAllow := cb.successCnt < cb.halfOpenMaxReqs
			cb.mu.RUnlock()
			return halfOpenAllow
		}
		return false
	case StateHalfOpen:
		cb.mu.RLock()
		halfOpenAllow := cb.successCnt < cb.halfOpenMaxReqs
		cb.mu.RUnlock()
		return halfOpenAllow
	default:
		return true
	}
}

// RecordSuccess 记录一次成功的推送操作
func (cb *FanoutCircuitBreaker) RecordSuccess(ctx context.Context, latency time.Duration) {
	// 同时检查延迟是否超过阈值
	if latency > cb.timeout {
		cb.RecordFailure("timeout: " + latency.String())
		return
	}

	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateHalfOpen:
		cb.successCnt++
		if cb.successCnt >= cb.successThreshold {
			cb.state = StateClosed
			cb.failureCnt = 0
			log.Printf("[CircuitBreaker] State transition: HALF_OPEN → CLOSED (recovered)")
		}
	case StateClosed:
		cb.failureCnt = 0 // 成功后重置失败计数
	}
}

// RecordFailure 记录一次失败的推送操作
func (cb *FanoutCircuitBreaker) RecordFailure(reason string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failureCnt++
	cb.lastFailAt = time.Now()
	cb.lastTripReason = reason

	if cb.state == StateHalfOpen || (cb.state == StateClosed && cb.failureCnt >= cb.failureThreshold) {
		from := cb.state.String()
		cb.state = StateOpen
		cb.totalTrips++
		log.Printf("[CircuitBreaker] State transition: %s → OPEN (reason: %s, failures: %d, totalTrips: %d)",
			from, reason, cb.failureCnt, cb.totalTrips)
	}
}

// State 返回当前状态
func (cb *FanoutCircuitBreaker) State() CircuitState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// Stats 返回熔断器统计信息
func (cb *FanoutCircuitBreaker) Stats() map[string]any {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return map[string]any{
		"state":            cb.state.String(),
		"failure_count":    cb.failureCnt,
		"total_trips":      cb.totalTrips,
		"last_trip_reason": cb.lastTripReason,
		"last_fail_at":     cb.lastFailAt,
	}
}

// ==================== 按作者隔离的熔断器注册表（P2）====================
//
// 修复前：全局只有一个熔断器。一个大V连续失败 3 次 → 熔断器 OPEN 30 秒 →
//
//	这 30 秒内**所有文章**（含小作者的）全部降级。
//	这不是性能问题，是可用性问题：QPS 低可以扩容，跨作者串扰没法扩容。
//
// 修复后：每个作者一个独立的熔断器，用 authorID 做 key。
//
//	大V打爆自己的熔断器只影响自己，小作者的推送照常。
//
// 内存保护：注册表有上限，超过时淘汰「状态为 CLOSED 且最久没失败」的条目。
// 只回收 CLOSED 的：把 OPEN 的熔断器扔掉等于给故障作者开了后门。
type AuthorCircuitRegistry struct {
	mu       sync.Mutex
	breakers map[uint]*FanoutCircuitBreaker
	maxSize  int
	timeout  time.Duration
}

// NewAuthorCircuitRegistry 创建按作者隔离的熔断器注册表
func NewAuthorCircuitRegistry(timeout time.Duration, maxSize int) *AuthorCircuitRegistry {
	if maxSize <= 0 {
		maxSize = 10000
	}
	return &AuthorCircuitRegistry{
		breakers: make(map[uint]*FanoutCircuitBreaker),
		maxSize:  maxSize,
		timeout:  timeout,
	}
}

// Get 取得某作者专属的熔断器，不存在则创建
func (r *AuthorCircuitRegistry) Get(authorID uint) *FanoutCircuitBreaker {
	r.mu.Lock()
	if cb, ok := r.breakers[authorID]; ok {
		r.mu.Unlock()
		return cb
	}

	// 容量保护：优先淘汰 CLOSED 且最久没失败过的（即最不可能有信息的）
	if len(r.breakers) >= r.maxSize {
		var victim uint
		var victimAt time.Time
		found := false
		for id, cb := range r.breakers {
			cb.mu.RLock()
			closed := cb.state == StateClosed
			failAt := cb.lastFailAt
			cb.mu.RUnlock()
			if !closed {
				continue
			}
			if !found || failAt.Before(victimAt) {
				victim, victimAt, found = id, failAt, true
			}
		}
		if found {
			delete(r.breakers, victim)
		}
		// 一个 CLOSED 都没有（全员熔断中）→ 不淘汰，宁可多占一点内存
	}

	cb := NewFanoutCircuitBreaker(r.timeout)
	r.breakers[authorID] = cb
	r.mu.Unlock()
	return cb
}

// Snapshot 返回当前所有处于非 CLOSED 状态的熔断器（供 /feed/stats 观测）
func (r *AuthorCircuitRegistry) Snapshot() map[uint]map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make(map[uint]map[string]any)
	for id, cb := range r.breakers {
		stats := cb.Stats()
		if stats["state"] != StateClosed.String() {
			out[id] = stats
		}
	}
	return out
}

// Size 当前注册的熔断器数量
func (r *AuthorCircuitRegistry) Size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.breakers)
}
