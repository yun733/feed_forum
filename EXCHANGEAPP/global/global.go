package global

import (
	"github.com/go-redis/redis/v8"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
)

var (
	Db           *gorm.DB
	RedisDB      *redis.Client
	RabbitMQCh   *amqp.Channel
	RabbitMQConn *amqp.Connection

	// MarkActiveFunc 由 controllers 注入，打破 middlewares→controllers 循环依赖
	// Auth 中间件调用此函数标记用户活跃（走本地滑动窗口，0 网络 IO）
	// 返回 true 表示「这是用户离开 7 天窗口后的第一次回来」—— 期间 fanout
	// 按维度二跳过了他，读路径需要强制重建 timeline
	MarkActiveFunc func(userID uint) bool
)
