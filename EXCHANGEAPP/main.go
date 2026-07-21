package main

import (
	"context"
	"exchangeapp/config"
	"exchangeapp/controllers"
	"exchangeapp/global"
	"exchangeapp/mq"
	"exchangeapp/router"
	"exchangeapp/services"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"
)

func main() {
	config.InitConfig()
	config.InitDB()
	config.InitRedis()
	mq.InitRabbitMQ(config.AppConfig.RabbitMQ.URL)

	// ========== 初始化 Feed 相关服务 ==========
	controllers.InitFeedServices()

	// ========== 启动 Outbox 定时扫描器（消息积压兜底）==========
	outboxSvc := services.NewOutboxService(5 * time.Second)
	outboxSvc.Start()

	// 注册 Outbox 兜底回调到 MQ 层
	mq.OutboxFallbackFunc = func(routingKey string, body []byte) error {
		return outboxSvc.Enqueue(routingKey, body, 10)
	}
	log.Println("[Main] Outbox fallback registered")

	// ========== 启动 RabbitMQ 消费者 ==========
	controllers.StartFeedConsumer()
	startNotifyConsumer()
	startLikeSyncConsumer()
	log.Println("[Main] All RabbitMQ consumers started")

	// ========== 启动 HTTP 服务 ==========
	r := router.SetupRouter()

	port := config.AppConfig.App.Port
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: r,
	}

	go func() {
		log.Printf("Server starting on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	// ========== 优雅关闭 ==========
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)
	<-quit
	log.Println("Shutting down server...")

	// 停止 Outbox 扫描器
	outboxSvc.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}

	if global.RabbitMQCh != nil {
		global.RabbitMQCh.Close()
	}
	if global.RabbitMQConn != nil {
		global.RabbitMQConn.Close()
	}

	log.Println("Server exiting")
}

// startNotifyConsumer 启动通知消费者
func startNotifyConsumer() {
	go controllers.ConsumeNotifications()
	log.Println("[Main] Notify consumer started")
}

// startLikeSyncConsumer 启动点赞同步消费者
func startLikeSyncConsumer() {
	go controllers.SyncLikesConsumer()
	log.Println("[Main] Like sync consumer started")
}
