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
	StateClosed    CircuitState = iota // 正常，请求放行
	StateOpen                          // 熔断，直接拒绝
	StateHalfOpen                      // 半开，尝试探测恢复
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

	state       CircuitState
	failureCnt  int
	successCnt  int
	lastFailAt  time.Time

	// 配置
	failureThreshold int           // 连续失败 N 次后熔断（默认 3）
	successThreshold int           // 半开状态下连续成功 N 次后恢复（默认 2）
	halfOpenMaxReqs  int           // 半开状态允许的最大探测请求数
	cooldown         time.Duration // 熔断冷却时间（默认 30s）
	timeout          time.Duration // Redis 操作超时阈值（默认 500ms）

	// 统计
	totalTrips     int64     // 总熔断次数
	lastTripReason string    // 最近一次熔断原因
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
		"state":           cb.state.String(),
		"failure_count":   cb.failureCnt,
		"total_trips":     cb.totalTrips,
		"last_trip_reason": cb.lastTripReason,
		"last_fail_at":    cb.lastFailAt,
	}
}
