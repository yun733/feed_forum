package services

import (
	"encoding/json"
	"exchangeapp/global"
	"exchangeapp/models"
	"exchangeapp/mq"
	"log"
	"math"
	"time"
)

// OutboxService 本地消息表服务
// 当 RabbitMQ 不可用时，消息写入 MySQL outbox 表
// 定时扫描 pending 消息，按指数退避策略重试
type OutboxService struct {
	scanInterval time.Duration // 扫描间隔（默认 5s）
	stopCh       chan struct{}
}

// NewOutboxService 创建 Outbox 服务
func NewOutboxService(scanInterval time.Duration) *OutboxService {
	if scanInterval <= 0 {
		scanInterval = 5 * time.Second
	}
	return &OutboxService{
		scanInterval: scanInterval,
		stopCh:       make(chan struct{}),
	}
}

// Enqueue 将消息写入 Outbox 表（RabbitMQ 推送失败时的兜底）
func (s *OutboxService) Enqueue(routingKey string, body []byte, maxRetries int) error {
	if maxRetries <= 0 {
		maxRetries = 10
	}
	msg := models.OutboxMessage{
		RoutingKey:  routingKey,
		Body:        string(body),
		Status:      "pending",
		RetryCount:  0,
		MaxRetries:  maxRetries,
		NextRetryAt: time.Now().Add(5 * time.Second), // 首次重试 5s 后
	}
	return global.Db.Create(&msg).Error
}

// EnqueueArticle 便捷方法：将文章推送消息写入 Outbox
func (s *OutboxService) EnqueueArticle(article models.Article) error {
	body, _ := json.Marshal(article)
	return s.Enqueue(mq.RoutingKeyFeedPush, body, 10)
}

// EnqueueNotification 便捷方法：将通知消息写入 Outbox
func (s *OutboxService) EnqueueNotification(routingKey string, body []byte) error {
	return s.Enqueue(routingKey, body, 10)
}

// Start 启动定时扫描器
func (s *OutboxService) Start() {
	log.Printf("[OutboxService] Started with scan interval: %v", s.scanInterval)
	go s.scanLoop()
}

// Stop 停止定时扫描器
func (s *OutboxService) Stop() {
	close(s.stopCh)
	log.Println("[OutboxService] Stopped")
}

// scanLoop 定时扫描 pending 消息并重试
func (s *OutboxService) scanLoop() {
	ticker := time.NewTicker(s.scanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.processBatch()
		case <-s.stopCh:
			return
		}
	}
}

// processBatch 处理一批待重试的消息
func (s *OutboxService) processBatch() {
	var messages []models.OutboxMessage

	// 查找需要重试的消息：pending 且 next_retry_at <= now
	global.Db.Where("status = ? AND next_retry_at <= ?", "pending", time.Now()).
		Order("next_retry_at ASC").
		Limit(50). // 每次最多处理 50 条
		Find(&messages)

	for _, msg := range messages {
		s.retryMessage(msg)
	}
}

// retryMessage 重试单条消息
func (s *OutboxService) retryMessage(msg models.OutboxMessage) {
	// 标记为 processing 防止重复处理
	global.Db.Model(&msg).Update("status", "processing")

	// 尝试发布到 RabbitMQ
	err := mq.Publish(msg.RoutingKey, []byte(msg.Body))
	if err == nil {
		// 成功 → 标记 done
		global.Db.Model(&msg).Updates(map[string]any{
			"status": "done",
		})
		log.Printf("[OutboxService] Message %d published successfully after %d retries", msg.ID, msg.RetryCount)
		return
	}

	// 失败 → 增加重试次数
	newRetryCount := msg.RetryCount + 1
	if newRetryCount >= msg.MaxRetries {
		// 超过最大重试次数 → 标记 failed
		global.Db.Model(&msg).Updates(map[string]any{
			"status":      "failed",
			"retry_count": newRetryCount,
			"error_msg":   err.Error(),
		})
		log.Printf("[OutboxService] Message %d failed after %d retries: %v", msg.ID, newRetryCount, err)
		return
	}

	// 指数退避：5s, 10s, 20s, 40s, 80s, 160s, 320s, 640s, 1280s, 2560s
	backoff := time.Duration(math.Pow(2, float64(newRetryCount))) * 5 * time.Second
	nextRetry := time.Now().Add(backoff)

	global.Db.Model(&msg).Updates(map[string]any{
		"status":        "pending", // 重置为 pending 等待下次扫描
		"retry_count":   newRetryCount,
		"next_retry_at": nextRetry,
		"error_msg":     err.Error(),
	})
	log.Printf("[OutboxService] Message %d retry %d/%d failed: %v, next retry at %v",
		msg.ID, newRetryCount, msg.MaxRetries, err, nextRetry)
}

// GetStats 获取 Outbox 统计信息
func (s *OutboxService) GetStats() map[string]any {
	var pending, processing, failed, done int64
	global.Db.Model(&models.OutboxMessage{}).Where("status = ?", "pending").Count(&pending)
	global.Db.Model(&models.OutboxMessage{}).Where("status = ?", "processing").Count(&processing)
	global.Db.Model(&models.OutboxMessage{}).Where("status = ?", "failed").Count(&failed)
	global.Db.Model(&models.OutboxMessage{}).Where("status = ?", "done").Count(&done)

	return map[string]any{
		"pending":    pending,
		"processing": processing,
		"failed":     failed,
		"done":       done,
	}
}
