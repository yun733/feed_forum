package controllers

import (
	"exchangeapp/global"
	"exchangeapp/models"
	"exchangeapp/utils"
	"time"

	"github.com/gin-gonic/gin"
)

func Register(ctx *gin.Context) {
	var user models.User
	if err := ctx.ShouldBindJSON(&user); err != nil {
		ctx.JSON(400, gin.H{"error": err.Error()})
		return
	}
	hashedPwd, err := utils.HashPassword(user.Password)
	if err != nil {
		ctx.JSON(500, gin.H{"error": err.Error()})
		return
	}
	user.Password = hashedPwd
	user.LastActiveAt = time.Now()

	if err := global.Db.Create(&user).Error; err != nil {
		ctx.JSON(500, gin.H{"error": err.Error()})
		return
	}

	token, err := utils.GenerateJWT(user.ID, user.Username)
	if err != nil {
		ctx.JSON(500, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(200, gin.H{
		"token": token,
		"user": gin.H{
			"id":       user.ID,
			"username": user.Username,
		},
	})
}

func Login(ctx *gin.Context) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := ctx.ShouldBindJSON(&input); err != nil {
		ctx.JSON(400, gin.H{"error": err.Error()})
		return
	}
	var user models.User
	if err := global.Db.Where("username = ?", input.Username).First(&user).Error; err != nil {
		ctx.JSON(401, gin.H{"error": "wrong username or password"})
		return
	}
	if !utils.CheckPasswordHash(input.Password, user.Password) {
		ctx.JSON(401, gin.H{"error": "wrong username or password"})
		return
	}

	// 更新最后活跃时间
	global.Db.Model(&user).Update("last_active_at", time.Now())

	token, err := utils.GenerateJWT(user.ID, user.Username)
	if err != nil {
		ctx.JSON(500, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(200, gin.H{
		"token": token,
		"user": gin.H{
			"id":       user.ID,
			"username": user.Username,
			"avatar":   user.Avatar,
		},
	})
}
