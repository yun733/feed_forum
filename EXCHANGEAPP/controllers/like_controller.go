package controllers

import (
	"encoding/json"
	"fmt"
	"exchangeapp/global"
	"exchangeapp/models"
	"exchangeapp/mq"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// LikeArticle 点赞/取消点赞文章，使用 Redis + Lua 原子操作
func LikeArticle(ctx *gin.Context) {
	userID := ctx.GetUint("userID")
	articleID := ctx.Param("id")
	likeKey := "article:" + articleID + ":likes"
	userLikeKey := "article:" + articleID + ":liked:" + strconv.Itoa(int(userID))

	// Lua 脚本：原子化检查 + toggle + 返回状态
	script := `
		local like_key = KEYS[1]
		local user_like_key = KEYS[2]
		local is_liked = redis.call('GET', user_like_key)
		if is_liked and is_liked == '1' then
			redis.call('DECR', like_key)
			redis.call('DEL', user_like_key)
			return {-1, redis.call('GET', like_key)}
		else
			redis.call('INCR', like_key)
			redis.call('SET', user_like_key, '1')
			redis.call('EXPIRE', user_like_key, 2592000)
			return {1, redis.call('GET', like_key)}
		end
	`

	result, err := global.RedisDB.Eval(ctx, script, []string{likeKey, userLikeKey}).Int64Slice()
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "like failed"})
		return
	}

	action := result[0]   // 1 = liked, -1 = unliked
	likeCount := result[1]

	// 异步同步到 MySQL + 发通知
	go func() {
		msg := map[string]interface{}{
			"user_id":    userID,
			"article_id": articleID,
			"action":     action,
		}
		body, _ := json.Marshal(msg)
		mq.PublishWithOutbox(mq.RoutingKeyLikeSync, body)
	}()

	ctx.JSON(http.StatusOK, gin.H{
		"liked":      action == 1,
		"like_count": likeCount,
	})
}

func GetArticleLikes(ctx *gin.Context) {
	articleID := ctx.Param("id")
	likeKey := "article:" + articleID + ":likes"
	likes, err := global.RedisDB.Get(ctx, likeKey).Int64()
	if err != nil {
		likes = 0
	}
	ctx.JSON(http.StatusOK, gin.H{"likes": likes})
}

// CheckLikeStatus 检查当前用户是否已点赞
func CheckLikeStatus(ctx *gin.Context) {
	userID := ctx.GetUint("userID")
	articleID := ctx.Param("id")
	userLikeKey := "article:" + articleID + ":liked:" + strconv.Itoa(int(userID))
	liked, _ := global.RedisDB.Get(ctx, userLikeKey).Result()
	ctx.JSON(http.StatusOK, gin.H{"liked": liked == "1"})
}

// LikeComment 点赞评论
func LikeComment(ctx *gin.Context) {
	userID := ctx.GetUint("userID")
	commentID := ctx.Param("id")
	likeKey := "comment:" + commentID + ":likes"
	userLikeKey := "comment:" + commentID + ":liked:" + strconv.Itoa(int(userID))

	script := `
		local like_key = KEYS[1]
		local user_like_key = KEYS[2]
		local is_liked = redis.call('GET', user_like_key)
		if is_liked and is_liked == '1' then
			redis.call('DECR', like_key)
			redis.call('DEL', user_like_key)
			return {-1, redis.call('GET', like_key)}
		else
			redis.call('INCR', like_key)
			redis.call('SET', user_like_key, '1')
			redis.call('EXPIRE', user_like_key, 2592000)
			return {1, redis.call('GET', like_key)}
		end
	`

	result, err := global.RedisDB.Eval(ctx, script, []string{likeKey, userLikeKey}).Int64Slice()
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "like failed"})
		return
	}

	// 同步更新 MySQL
	cid, _ := strconv.ParseUint(commentID, 10, 64)
	if result[0] == 1 {
		global.Db.Model(&models.Comment{}).Where("id = ?", cid).
			UpdateColumn("like_count", global.Db.Raw("like_count + 1"))
	} else {
		global.Db.Model(&models.Comment{}).Where("id = ?", cid).
			UpdateColumn("like_count", global.Db.Raw("GREATEST(like_count - 1, 0)"))
	}

	ctx.JSON(http.StatusOK, gin.H{
		"liked":      result[0] == 1,
		"like_count": result[1],
	})
}

// SyncLikesConsumer RabbitMQ消费者：同步点赞数据到MySQL
func SyncLikesConsumer() {
	ch, _ := global.RabbitMQConn.Channel()
	q, _ := ch.QueueDeclare(mq.LikeSyncQueue, true, false, false, false, mq.DLXTable(mq.LikeSyncQueue))
	ch.QueueBind(q.Name, mq.RoutingKeyLikeSync, mq.ExchangeName, false, nil)
	msgs, _ := ch.Consume(q.Name, "", false, false, false, false, nil)

	for msg := range msgs {
		var data struct {
			UserID    uint   `json:"user_id"`
			ArticleID string `json:"article_id"`
			Action    int64  `json:"action"`
		}
		json.Unmarshal(msg.Body, &data)

		aid, _ := strconv.ParseUint(data.ArticleID, 10, 64)

		if data.Action == 1 {
			// 创建点赞记录
			like := models.Like{
				UserID:     data.UserID,
				TargetType: "article",
				TargetID:   uint(aid),
			}
			global.Db.Where(like).FirstOrCreate(&like)

			// 更新文章点赞数
			global.Db.Model(&models.Article{}).Where("id = ?", aid).
				UpdateColumn("like_count", global.Db.Raw("like_count + 1"))

			// 发通知
			sendLikeNotification(data.UserID, uint(aid))
		} else {
			global.Db.Where("user_id = ? AND target_type = ? AND target_id = ?",
				data.UserID, "article", uint(aid)).Delete(&models.Like{})
			global.Db.Model(&models.Article{}).Where("id = ?", aid).
				UpdateColumn("like_count", global.Db.Raw("GREATEST(like_count - 1, 0)"))
		}

		msg.Ack(false)
		fmt.Printf("Like synced: user=%d article=%s action=%d\n", data.UserID, data.ArticleID, data.Action)
	}
}

func sendLikeNotification(fromUserID, articleID uint) {
	var article models.Article
	global.Db.Select("author_id", "title").First(&article, articleID)
	if article.AuthorID == fromUserID {
		return // 不给自己发通知
	}
	notif := map[string]interface{}{
		"user_id":      article.AuthorID,
		"from_user_id": fromUserID,
		"type":         "like",
		"target_id":    articleID,
		"content":      "赞了你的文章",
	}
	body, _ := json.Marshal(notif)
	mq.PublishWithOutbox("notification.like", body)
}
