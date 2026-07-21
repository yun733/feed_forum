package mq

import (
	"exchangeapp/global"
	"log"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	ExchangeName  = "zhihu.topic"
	FeedPushQueue = "feed.push.queue"
	NotifyQueue   = "notification.queue"
	LikeSyncQueue = "like.sync.queue"

	RoutingKeyFeedPush = "feed.push.article"
	RoutingKeyNotify   = "notification.*"
	RoutingKeyLikeSync = "like.sync"
)

// OutboxFallbackFunc Outbox 兜底回调：当 Publish 失败时调用
var OutboxFallbackFunc func(routingKey string, body []byte) error

func InitRabbitMQ(url string) {
	conn, err := amqp.Dial(url)
	if err != nil {
		log.Fatalf("failed to connect to RabbitMQ: %v", err)
	}
	global.RabbitMQConn = conn

	ch, err := conn.Channel()
	if err != nil {
		log.Fatalf("failed to open channel: %v", err)
	}

	if err := ch.ExchangeDeclare(
		ExchangeName, "topic", true, false, false, false, nil,
	); err != nil {
		log.Fatalf("failed to declare exchange: %v", err)
	}

	global.RabbitMQCh = ch

	declareQueue(ch, FeedPushQueue, RoutingKeyFeedPush)
	declareQueue(ch, NotifyQueue, RoutingKeyNotify)
	declareQueue(ch, LikeSyncQueue, RoutingKeyLikeSync)

	// 启用 Publisher Confirms
	if err := ch.Confirm(false); err != nil {
		log.Fatalf("failed to enable publisher confirms: %v", err)
	}

	log.Println("RabbitMQ initialized successfully")
}

func declareQueue(ch *amqp.Channel, queueName, routingKey string) {
	q, err := ch.QueueDeclare(
		queueName, true, false, false, false,
		amqp.Table{
			"x-dead-letter-exchange":    ExchangeName + ".dlx",
			"x-dead-letter-routing-key": queueName + ".dead",
		},
	)
	if err != nil {
		log.Fatalf("failed to declare queue %s: %v", queueName, err)
	}
	if err := ch.QueueBind(q.Name, routingKey, ExchangeName, false, nil); err != nil {
		log.Fatalf("failed to bind queue %s: %v", queueName, err)
	}
}

// Publish 发布消息到 RabbitMQ（阻塞等待确认）
func Publish(routingKey string, body []byte) error {
	confirms := global.RabbitMQCh.NotifyPublish(make(chan amqp.Confirmation, 1))
	err := global.RabbitMQCh.Publish(
		ExchangeName, routingKey, true, false,
		amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Body:         body,
		},
	)
	if err != nil {
		return err
	}
	if confirmed := <-confirms; !confirmed.Ack {
		return amqp.ErrClosed
	}
	return nil
}

// PublishWithOutbox 发布消息，失败时自动写入 Outbox 兜底
// 这是所有业务代码应该调用的方法，确保消息不丢失
func PublishWithOutbox(routingKey string, body []byte) {
	err := Publish(routingKey, body)
	if err == nil {
		return
	}

	log.Printf("[MQ] Publish failed for routingKey=%s: %v, falling back to outbox", routingKey, err)

	// 写入 Outbox 表兜底
	if OutboxFallbackFunc != nil {
		if fallbackErr := OutboxFallbackFunc(routingKey, body); fallbackErr != nil {
			log.Printf("[MQ] Outbox fallback also failed for routingKey=%s: %v", routingKey, fallbackErr)
		} else {
			log.Printf("[MQ] Message saved to outbox for routingKey=%s", routingKey)
		}
	}
}
