package models

import (
	"time"

	"gorm.io/gorm"
)

// OutboxMessage 本地延迟消息表，RabbitMQ 推送失败时兜底
// 定时扫描 pending 状态的消息，按指数退避重试
type OutboxMessage struct {
	gorm.Model
	RoutingKey  string    `gorm:"size:100;index:idx_outbox_routing"`
	Body        string    `gorm:"type:longtext;not null"`
	Status      string    `gorm:"size:20;index:idx_outbox_status;default:pending"` // pending / processing / done / failed
	RetryCount  int       `gorm:"default:0"`
	MaxRetries  int       `gorm:"default:10"`
	NextRetryAt time.Time `gorm:"index:idx_outbox_next_retry"`
	ErrorMsg    string    `gorm:"size:500"`
}

// TableName 自定义表名
func (OutboxMessage) TableName() string {
	return "outbox_messages"
}
