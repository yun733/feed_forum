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
		refreshLastActive(claims.UserID)

		ctx.Next()
	}
}

// refreshLastActive 节流更新用户最后活跃时间
// 双层写入策略：
//   Layer 1（Redis）: SET user:active:<id> = 1, TTL 7天  ← Feed 推送/拉取都读这个
//   Layer 2（MySQL）: UPDATE users SET last_active_at     ← 持久化兜底
// Redis SETNX 5分钟节流，避免每次请求都写 MySQL
func refreshLastActive(userID uint) {
	throttleKey := fmt.Sprintf("user:active:throttle:%d", userID)

	// SET NX + EX：5 分钟内重复请求直接跳过
	ok, err := global.RedisDB.SetNX(context.Background(), throttleKey, "1", 5*time.Minute).Result()
	if err != nil || !ok {
		return
	}

	// 优先走本地滑动窗口（0 网络 IO → 批量 flush 到 Redis）
	if global.MarkActiveFunc != nil {
		global.MarkActiveFunc(userID)
	} else {
		// 降级：直接写 Redis
		global.RedisDB.Set(context.Background(),
			fmt.Sprintf("user:active:%d", userID), "1", 7*24*time.Hour)
	}

	// 异步更新 MySQL（持久化兜底）
	go func() {
		global.Db.Table("users").
			Where("id = ?", userID).
			Update("last_active_at", time.Now())
	}()
}
