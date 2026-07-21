package middlewares

import (
	"exchangeapp/global"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// TokenBucketRateLimiter 使用 Redis 实现分布式令牌桶限流
// key: rate_limit:<identifier>  bucket: rate_limit:bucket:<identifier>
func TokenBucketRateLimiter(rate int, burst int) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		identifier := ctx.ClientIP()
		key := "rate_limit:" + identifier + ":tokens"
		lastRefillKey := "rate_limit:" + identifier + ":last_refill"

		now := time.Now().UnixMilli()
		refillInterval := int64(1000 / rate) // 每个令牌间隔毫秒

		// Lua 脚本实现令牌桶算法
		script := `
			local tokens_key = KEYS[1]
			local last_refill_key = KEYS[2]
			local now = tonumber(ARGV[1])
			local rate = tonumber(ARGV[2])
			local burst = tonumber(ARGV[3])
			local refill_interval = tonumber(ARGV[4])

			local tokens = tonumber(redis.call('GET', tokens_key) or burst)
			local last_refill = tonumber(redis.call('GET', last_refill_key) or now)

			local elapsed = now - last_refill
			local new_tokens = math.floor(elapsed / refill_interval)
			if new_tokens > 0 then
				tokens = math.min(tokens + new_tokens, burst)
				redis.call('SET', last_refill_key, now)
			end

			if tokens >= 1 then
				redis.call('SET', tokens_key, tokens - 1)
				redis.call('EXPIRE', tokens_key, 60)
				redis.call('EXPIRE', last_refill_key, 60)
				return 1
			else
				redis.call('SET', tokens_key, tokens)
				redis.call('EXPIRE', tokens_key, 60)
				return 0
			end
		`

		result, err := global.RedisDB.Eval(ctx, script, []string{key, lastRefillKey},
			now, rate, burst, refillInterval).Int()
		if err != nil || result == 0 {
			ctx.JSON(http.StatusTooManyRequests, gin.H{
				"error": "rate limit exceeded, try again later",
			})
			ctx.Abort()
			return
		}
		ctx.Next()
	}
}
