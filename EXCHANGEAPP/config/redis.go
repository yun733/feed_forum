package config

import (
	"context"
	"exchangeapp/global"
	"log"

	"github.com/go-redis/redis/v8"
)

func InitRedis() {
	RedisClient := redis.NewClient(&redis.Options{
		Addr:     AppConfig.Redis.Addr,
		Password: AppConfig.Redis.Password,
		DB:       AppConfig.Redis.DB,
	})
	if _, err := RedisClient.Ping(context.Background()).Result(); err != nil {
		log.Fatalf("failed to connect to Redis: %v", err)
	}
	global.RedisDB = RedisClient
}
