package controllers

import (
	"encoding/json"
	"exchangeapp/global"
	"exchangeapp/models"
	"exchangeapp/mq"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

func GetNotifications(ctx *gin.Context) {
	userID := ctx.GetUint("userID")
	page, _ := strconv.Atoi(ctx.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(ctx.DefaultQuery("size", "20"))
	offset := (page - 1) * size

	var notifications []models.Notification
	var total int64

	global.Db.Model(&models.Notification{}).Where("user_id = ?", userID).Count(&total)
	global.Db.Where("user_id = ?", userID).
		Preload("FromUser").
		Order("created_at DESC").
		Offset(offset).Limit(size).
		Find(&notifications)

	ctx.JSON(http.StatusOK, gin.H{
		"items": notifications,
		"total": total,
	})
}

func MarkNotificationRead(ctx *gin.Context) {
	userID := ctx.GetUint("userID")
	id := ctx.Param("id")

	global.Db.Model(&models.Notification{}).
		Where("id = ? AND user_id = ?", id, userID).
		Update("is_read", true)

	ctx.JSON(http.StatusOK, gin.H{"message": "marked as read"})
}

func GetUnreadCount(ctx *gin.Context) {
	userID := ctx.GetUint("userID")
	var count int64
	global.Db.Model(&models.Notification{}).
		Where("user_id = ? AND is_read = ?", userID, false).
		Count(&count)

	ctx.JSON(http.StatusOK, gin.H{"unread_count": count})
}

// ConsumeNotifications RabbitMQ消费方：异步创建通知
func ConsumeNotifications() {
	ch, _ := global.RabbitMQConn.Channel()
	q, _ := ch.QueueDeclare(mq.NotifyQueue, true, false, false, false, nil)
	ch.QueueBind(q.Name, mq.RoutingKeyNotify, mq.ExchangeName, false, nil)
	msgs, _ := ch.Consume(q.Name, "", false, false, false, false, nil)

	for msg := range msgs {
		var data struct {
			UserID     uint   `json:"user_id"`
			FromUserID uint   `json:"from_user_id"`
			Type       string `json:"type"`
			TargetID   uint   `json:"target_id"`
			Content    string `json:"content"`
		}
		if err := json.Unmarshal(msg.Body, &data); err != nil {
			msg.Ack(false)
			continue
		}

		// 不给自己发通知
		if data.UserID == data.FromUserID {
			msg.Ack(false)
			continue
		}

		notif := models.Notification{
			UserID:     data.UserID,
			FromUserID: data.FromUserID,
			Type:       data.Type,
			TargetID:   data.TargetID,
			Content:    data.Content,
		}
		global.Db.Create(&notif)
		msg.Ack(false)
	}
}
