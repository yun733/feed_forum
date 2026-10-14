package middlewares

import (
	"context"
	"exchangeapp/global"
	"exchangeapp/utils"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

func AuthMiddleware() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		token := ctx.GetHeader("Authorization")
		if token == "" {
			ctx.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
			ctx.Abort()
			return
		}
		claims, err := utils.ParseJWT(token)
		if err != nil {
			ctx.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			ctx.Abort()
			return
		}
		ctx.Set("userID", claims.UserID)
		ctx.Set("username", claims.Username)

		// 活跃时间节流更新：Redis key 做去重，5分钟内不重复写 DB
		// 返回值表示「这是不是离开 7 天窗口后的第一次回来」，透传给 Feed 读路径
		ctx.Set("feedReturning", refreshLastActive(claims.UserID))

		ctx.Next()
	}
}

// refreshLastActive 节流更新用户最后活跃时间，并返回「本次是不是离开 7 天窗口后的第一次回来」。
//
// 双层写入：Layer 1 写 Redis（user:active:<id>，TTL 7 天，Feed 推送/拉取都读它），
// Layer 2 写 MySQL（users.last_active_at，持久化兜底）。
// 用 Redis SETNX 做 5 分钟节流，避免每次请求都写 MySQL。
//
// 返回值是免费的：节流键决定了这里每个用户最多 5 分钟来一次，
// 所以判断「回来」多出来的那次 EXISTS 被摊到 5 分钟上，而不是每个请求一次。
func refreshLastActive(userID uint) bool {
	throttleKey := fmt.Sprintf("user:active:throttle:%d", userID)

	// SET NX + EX：5 分钟内重复请求直接跳过
	ok, err := global.RedisDB.SetNX(context.Background(), throttleKey, "1", 5*time.Minute).Result()
	if err != nil || !ok {
		return false
	}

	// 异步更新 MySQL（持久化兜底）
	updateLastActive := func() {
		go func() {
			global.Db.Table("users").
				Where("id = ?", userID).
				Update("last_active_at", time.Now())
		}()
	}

	// 优先走本地滑动窗口（0 网络 IO → 批量 flush 到 Redis）
	if global.MarkActiveFunc != nil {
		returning := global.MarkActiveFunc(userID)
		updateLastActive()
		return returning
	}

	// 降级：直接写 Redis
	activeKey := fmt.Sprintf("user:active:%d", userID)
	exists, _ := global.RedisDB.Exists(context.Background(), activeKey).Result()
	global.RedisDB.Set(context.Background(), activeKey, "1", 7*24*time.Hour)
	updateLastActive()
	return exists == 0
}
