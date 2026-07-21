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
	MarkActiveFunc func(userID uint)
)
