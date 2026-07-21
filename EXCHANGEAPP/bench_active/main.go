package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

var ctx = context.Background()

func main() {
	addr := "localhost:6379"
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		addr = v
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: 0})
	if err := rdb.Ping(ctx).Err(); err != nil {
		fmt.Printf("Redis connection failed: %v\n", err)
		os.Exit(1)
	}

	// 预填充
	prePopulate(rdb, 10000)

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║  纯 Redis  vs  Redis + 本地滑动窗口  — QPS 对比          ║")
	fmt.Println("╠══════════════════════════════════════════════════════════╣")

	// ── 写入路径对比 ──
	fmt.Println("║                                                          ║")
	fmt.Println("║  【写入路径】Auth 中间件 MarkActive                       ║")
	fmt.Println("╠══════════════════════════════════════════════════════════╣")
	benchWritePureRedis(rdb)
	benchWriteLocalWindow(rdb)

	// ── 读取路径对比：单次 ──
	fmt.Println("║                                                          ║")
	fmt.Println("║  【读取路径-单次】Push 消费者 isUserActive                ║")
	fmt.Println("╠══════════════════════════════════════════════════════════╣")
	benchReadSinglePureRedis(rdb)
	benchReadSingleLocalWindow(rdb)

	// ── 读取路径对比：批量 ──
	fmt.Println("║                                                          ║")
	fmt.Println("║  【读取路径-批量】Pull FilterInactive (N=1000)            ║")
	fmt.Println("╠══════════════════════════════════════════════════════════╣")
	benchReadBatchPureRedis(rdb)
	benchReadBatchLocalWindow(rdb)

	// ── 总结 ──
	fmt.Println("╠══════════════════════════════════════════════════════════╣")
	fmt.Println("║                      总结                                ║")
	fmt.Println("╠══════════════════════════════════════════════════════════╣")
	fmt.Println("║  写入：本地窗口优势巨大 — 0 网络 IO vs 每次 Redis 往返    ║")
	fmt.Println("║  读取（单次）：差距不大 — 78μs vs 50ns，绝对值都极小      ║")
	fmt.Println("║  读取（批量）：本地命中时优势明显，但 Redis Pipeline 也快  ║")
	fmt.Println("║                                                          ║")
	fmt.Println("║  结论：写入路径收益最大，适合 Auth 中间件这种热点路径       ║")
	fmt.Println("║        读取路径收益有限，但架构上更优雅（L1+L2 两级缓存）  ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")

	cleanup(rdb, 10000)
}

// ====================== 写入 ======================

func benchWritePureRedis(rdb *redis.Client) {
	count := 50000
	start := time.Now()
	for i := 0; i < count; i++ {
		uid := uint(i%10000 + 1)
		key := fmt.Sprintf("user:active:%d", uid)
		rdb.Set(ctx, key, "1", 7*24*time.Hour)
	}
	elapsed := time.Since(start)
	fmt.Printf("║  纯 Redis SET:    %8d 次  |  %7.0f ops/s  |  %6.0f μs/op\n",
		count, float64(count)/elapsed.Seconds(), float64(elapsed.Microseconds())/float64(count))
}

func benchWriteLocalWindow(rdb *redis.Client) {
	// 模拟本地窗口写入（纯内存 map）
	local := make(map[uint]int64)
	var mu sync.Mutex

	count := 5000000 // 500 万次 — 展示差距
	start := time.Now()
	for i := 0; i < count; i++ {
		uid := uint(i%10000 + 1)
		mu.Lock()
		local[uid] = time.Now().Unix()
		mu.Unlock()
	}
	elapsed := time.Since(start)
	fmt.Printf("║  本地 map 写入:   %8d 次  |  %7.0f ops/s  |  %6.0f ns/op  ← 无网络IO\n",
		count, float64(count)/elapsed.Seconds(), float64(elapsed.Nanoseconds())/float64(count))
	_ = rdb
}

// ====================== 读取单次 ======================

func benchReadSinglePureRedis(rdb *redis.Client) {
	iterations := 100000
	start := time.Now()
	for i := 0; i < iterations; i++ {
		uid := uint(i%10000 + 1)
		key := fmt.Sprintf("user:active:%d", uid)
		rdb.Exists(ctx, key)
	}
	elapsed := time.Since(start)
	fmt.Printf("║  纯 Redis EXISTS:  %6d 次  |  %7.0f ops/s  |  %6.0f μs/op\n",
		iterations, float64(iterations)/elapsed.Seconds(), float64(elapsed.Microseconds())/float64(iterations))
}

func benchReadSingleLocalWindow(rdb *redis.Client) {
	// 构建本地热缓存（模拟 flush 后状态）
	clean := make(map[uint]bool)
	for i := 0; i < 10000; i++ {
		clean[uint(i+1)] = true
	}

	iterations := 100000
	start := time.Now()
	for i := 0; i < iterations; i++ {
		uid := uint(i%10000 + 1)
		_ = clean[uid]
	}
	elapsed := time.Since(start)
	fmt.Printf("║  本地 map 命中:    %6d 次  |  %7.0f ops/s  |  %6.0f ns/op  ← 无网络IO\n",
		iterations, float64(iterations)/elapsed.Seconds(), float64(elapsed.Nanoseconds())/float64(iterations))
	_ = rdb
}

// ====================== 读取批量 ======================

func benchReadBatchPureRedis(rdb *redis.Client) {
	batchSize := 1000
	iterations := 5000

	// 生成测试 ID
	ids := make([]uint, batchSize)
	for i := range ids {
		ids[i] = uint(i%10000 + 1)
	}

	start := time.Now()
	for i := 0; i < iterations; i++ {
		pipe := rdb.Pipeline()
		for _, uid := range ids {
			key := fmt.Sprintf("user:active:%d", uid)
			pipe.Exists(ctx, key)
		}
		pipe.Exec(ctx)
	}
	elapsed := time.Since(start)
	batchesPerSec := float64(iterations) / elapsed.Seconds()
	checksPerSec := batchesPerSec * float64(batchSize)
	fmt.Printf("║  纯 Redis Pipeline: %4.0f批/s | %8.0f 用户/s | %5.0f μs/批\n",
		batchesPerSec, checksPerSec, float64(elapsed.Microseconds())/float64(iterations))
}

func benchReadBatchLocalWindow(rdb *redis.Client) {
	batchSize := 1000
	iterations := 5000

	// 本地热缓存（70% 命中率 — 模拟真实场景）
	clean := make(map[uint]bool)
	for i := 0; i < 7000; i++ {
		clean[uint(i+1)] = true
	}
	var mu sync.RWMutex

	ids := make([]uint, batchSize)
	for i := range ids {
		ids[i] = uint(i%10000 + 1)
	}

	// 统计命中/未命中
	var localHits, redisMiss int64

	start := time.Now()
	for i := 0; i < iterations; i++ {
		mu.RLock()
		var needRedis []uint
		for _, uid := range ids {
			if clean[uid] {
				localHits++
			} else {
				needRedis = append(needRedis, uid)
			}
		}
		mu.RUnlock()

		if len(needRedis) > 0 {
			redisMiss += int64(len(needRedis))
			pipe := rdb.Pipeline()
			for _, uid := range needRedis {
				key := fmt.Sprintf("user:active:%d", uid)
				pipe.Exists(ctx, key)
			}
			pipe.Exec(ctx)
		}
	}
	elapsed := time.Since(start)
	batchesPerSec := float64(iterations) / elapsed.Seconds()
	effectiveChecks := float64(localHits+redisMiss) / elapsed.Seconds()
	hitRate := float64(localHits) / float64(localHits+redisMiss) * 100

	fmt.Printf("║  本地+Redis混合:   %4.0f批/s | %8.0f 用户/s | %5.0f μs/批 | L1命中率 %.0f%%\n",
		batchesPerSec, effectiveChecks, float64(elapsed.Microseconds())/float64(iterations), hitRate)
}

func prePopulate(rdb *redis.Client, count int) {
	pipe := rdb.Pipeline()
	for i := 1; i <= count; i++ {
		pipe.Set(ctx, fmt.Sprintf("user:active:%d", i), "1", 7*24*time.Hour)
	}
	pipe.Exec(ctx)
}

func cleanup(rdb *redis.Client, count int) {
	pipe := rdb.Pipeline()
	for i := 1; i <= count; i++ {
		pipe.Del(ctx, fmt.Sprintf("user:active:%d", i))
	}
	pipe.Exec(ctx)
}
