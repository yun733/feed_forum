package controllers

import (
	"exchangeapp/global"
	"exchangeapp/models"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

func GetUserProfile(ctx *gin.Context) {
	id := ctx.Param("id")
	uid, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}

	var user models.User
	if err := global.Db.First(&user, uid).Error; err != nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	// 统计数据
	var articleCount, followerCount, followingCount int64
	global.Db.Model(&models.Article{}).Where("author_id = ? AND status = ?", uid, "published").Count(&articleCount)
	global.Db.Model(&models.Follow{}).Where("following_id = ?", uid).Count(&followerCount)
	global.Db.Model(&models.Follow{}).Where("follower_id = ?", uid).Count(&followingCount)

	ctx.JSON(http.StatusOK, gin.H{
		"id":             user.ID,
		"username":       user.Username,
		"avatar":         user.Avatar,
		"bio":            user.Bio,
		"article_count":  articleCount,
		"follower_count": followerCount,
		"following_count": followingCount,
		"created_at":     user.CreatedAt,
	})
}

func UpdateProfile(ctx *gin.Context) {
	userID := ctx.GetUint("userID")
	var input struct {
		Avatar string `json:"avatar"`
		Bio    string `json:"bio"`
	}
	if err := ctx.ShouldBindJSON(&input); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updates := map[string]interface{}{}
	if input.Avatar != "" {
		updates["avatar"] = input.Avatar
	}
	if input.Bio != "" {
		updates["bio"] = input.Bio
	}

	if len(updates) == 0 {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	if err := global.Db.Model(&models.User{}).Where("id = ?", userID).Updates(updates).Error; err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"message": "profile updated"})
}

func GetUserArticles(ctx *gin.Context) {
	id := ctx.Param("id")
	uid, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}

	page, _ := strconv.Atoi(ctx.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(ctx.DefaultQuery("size", "10"))
	offset := (page - 1) * size

	var articles []models.Article
	var total int64

	global.Db.Model(&models.Article{}).Where("author_id = ? AND status = ?", uid, "published").Count(&total)
	global.Db.Where("author_id = ? AND status = ?", uid, "published").
		Order("created_at DESC").Offset(offset).Limit(size).Find(&articles)

	ctx.JSON(http.StatusOK, gin.H{
		"items": articles,
		"total": total,
		"page":  page,
		"size":  size,
	})
}
