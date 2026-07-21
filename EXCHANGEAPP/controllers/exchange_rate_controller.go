package controllers

import (
	"exchangeapp/models"
	"net/http"
	"time"
	"exchangeapp/global"
	"github.com/gin-gonic/gin"
	"errors"
	"gorm.io/gorm"
)

func CreateExchangeRate(ctx *gin.Context) {
	// 创建一个ExchangeRate实例
	var exchangeRate models.ExchangeRate // 定义一个ExchangeRate变量用于接收请求数据
	if err := ctx.ShouldBindJSON(&exchangeRate); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	exchangeRate.Date = time.Now() // 设置当前时间为记录的日期

	// 保存到数据库
	if err := global.Db.AutoMigrate(&exchangeRate); err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := global.Db.Create(&exchangeRate).Error; err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	ctx.JSON(http.StatusOK, exchangeRate)
}
func GetExchangeRates(ctx *gin.Context){
	var exchangeRates []models.ExchangeRate//定义一个切片用于存储查询结果,每个元素都是一个ExchangeRate结构体
	if err := global.Db.Find(&exchangeRates).Error; err != nil {//查询数据库中的所有ExchangeRate记录,并将结果存储在exchangeRates切片中
		if errors.Is(err, gorm.ErrRecordNotFound) {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "No exchange rates found"})
			return
		}
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, exchangeRates)
}