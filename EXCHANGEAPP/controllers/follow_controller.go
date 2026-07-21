package controllers

import (
	"context"
	"encoding/json"
	"exchangeapp/global"
	"exchangeapp/models"
	"exchangeapp/mq"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

var followCtx = context.Background()

func FollowUser(ctx *gin.Context) {
	userID := ctx.GetUint("userID")
	targetIDStr := ctx.Param("id")
	targetID, err := strconv.ParseUint(targetIDStr, 10, 64)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}
	if uint(targetID) == userID {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "cannot follow yourself"})
		return
	}

	var follow models.Follow
	result := global.Db.Where("follower_id = ? AND following_id = ?", userID, targetID).First(&follow)
	if result.Error == nil {
		global.Db.Delete(&follow)
		global.Db.Model(&models.Follow{}).
			Where("follower_id = ? AND following_id = ?", targetID, userID).
			Update("is_mutual", false)
		// 取关时失效双方缓存 + 粉丝数缓存
		invalidateFollowCaches(userID, uint(targetID))
		ctx.JSON(http.StatusOK, gin.H{"is_following": false, "message": "unfollowed"})
		return
	}

	follow = models.Follow{
		FollowerID:  userID,
		FollowingID: uint(targetID),
	}
	global.Db.Create(&follow)

	var reverse models.Follow
	if global.Db.Where("follower_id = ? AND following_id = ?", targetID, userID).First(&reverse).Error == nil {
		global.Db.Model(&follow).Update("is_mutual", true)
		global.Db.Model(&reverse).Update("is_mutual", true)
	}

	// 关注时失效缓存 + 粉丝数缓存
	invalidateFollowCaches(userID, uint(targetID))

	go sendFollowNotification(userID, uint(targetID))

	ctx.JSON(http.StatusOK, gin.H{"is_following": true, "message": "followed"})
}

// invalidateFollowCaches 失效关注相关的所有缓存
func invalidateFollowCaches(followerID, followingID uint) {
	// 失效本地缓存（Feed Pull Service）
	if FeedPullSvc != nil {
		FeedPullSvc.InvalidateFollowingCache(followerID)
	}

	// 失效 Redis 粉丝数缓存
	followerCountKey := "user:" + strconv.Itoa(int(followingID)) + ":follower_count"
	global.RedisDB.Del(followCtx, followerCountKey)
}

func sendFollowNotification(fromUserID, toUserID uint) {
	notif := map[string]interface{}{
		"user_id":      toUserID,
		"from_user_id": fromUserID,
		"type":         "follow",
		"target_id":    0,
		"content":      "关注了你",
	}
	body, _ := json.Marshal(notif)
	mq.PublishWithOutbox("notification.follow", body)
}

func GetFollowers(ctx *gin.Context) {
	userID := ctx.Param("id")
	page, _ := strconv.Atoi(ctx.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(ctx.DefaultQuery("size", "20"))
	offset := (page - 1) * size

	var follows []models.Follow
	var total int64

	global.Db.Model(&models.Follow{}).Where("following_id = ?", userID).Count(&total)
	global.Db.Where("following_id = ?", userID).
		Offset(offset).Limit(size).
		Find(&follows)

	var users []gin.H
	for _, f := range follows {
		var user models.User
		global.Db.First(&user, f.FollowerID)
		users = append(users, gin.H{
			"id":       user.ID,
			"username": user.Username,
			"avatar":   user.Avatar,
		})
	}

	ctx.JSON(http.StatusOK, gin.H{
		"items": users,
		"total": total,
	})
}

func GetFollowing(ctx *gin.Context) {
	userID := ctx.Param("id")
	page, _ := strconv.Atoi(ctx.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(ctx.DefaultQuery("size", "20"))
	offset := (page - 1) * size

	var follows []models.Follow
	var total int64

	global.Db.Model(&models.Follow{}).Where("follower_id = ?", userID).Count(&total)
	global.Db.Where("follower_id = ?", userID).
		Offset(offset).Limit(size).
		Find(&follows)

	var users []gin.H
	for _, f := range follows {
		var user models.User
		global.Db.First(&user, f.FollowingID)
		users = append(users, gin.H{
			"id":       user.ID,
			"username": user.Username,
			"avatar":   user.Avatar,
		})
	}

	ctx.JSON(http.StatusOK, gin.H{
		"items": users,
		"total": total,
	})
}

func CheckFollowStatus(ctx *gin.Context) {
	userID := ctx.GetUint("userID")
	targetID := ctx.Param("id")

	var follow models.Follow
	if global.Db.Where("follower_id = ? AND following_id = ?", userID, targetID).First(&follow).Error == nil {
		ctx.JSON(http.StatusOK, gin.H{"is_following": true})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"is_following": false})
}
