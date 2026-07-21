package controllers

import (
	"encoding/json"
	"exchangeapp/global"
	"exchangeapp/models"
	"exchangeapp/mq"
	"exchangeapp/utils"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

var (
	sensitiveFilter = utils.NewSensitiveFilter([]string{
		"ad", "spam",
	})
)

var cacheProtector *utils.CacheProtector

func ensureCacheProtector() *utils.CacheProtector {
	if cacheProtector == nil {
		cacheProtector = utils.NewCacheProtector(global.RedisDB)
	}
	return cacheProtector
}

func CreateArticle(ctx *gin.Context) {
	userID := ctx.GetUint("userID")
	var article models.Article
	if err := ctx.ShouldBindJSON(&article); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	article.AuthorID = userID
	article.Title = sensitiveFilter.Replace(article.Title)
	article.Content = sensitiveFilter.Replace(article.Content)
	article.Preview = sensitiveFilter.Replace(article.Preview)

	if article.Status == "" {
		article.Status = "published"
	}

	if err := global.Db.Create(&article).Error; err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	global.Db.Preload("Author").First(&article, article.ID)

	// 清除文章列表缓存（模糊删除）
	keys, _ := global.RedisDB.Keys(ctx, "articles:list:*").Result()
	if len(keys) > 0 {
		global.RedisDB.Del(ctx, keys...)
	}

	if article.Status == "published" {
		go pushArticleToFeed(article)
	}

	ctx.JSON(http.StatusOK, article)
}

// pushArticleToFeed 文章发布后的推送入口
// 优化：使用 PublishWithOutbox 确保消息不丢失
// 实际 Fanout 逻辑在 ConsumeFeedPush 中由 BigVFanoutService 处理
func pushArticleToFeed(article models.Article) {
	body, _ := json.Marshal(article)
	mq.PublishWithOutbox(mq.RoutingKeyFeedPush, body)
}

// ==================== GetArticles: 互斥锁防击穿 ====================
func GetArticles(ctx *gin.Context) {
	page, _ := strconv.Atoi(ctx.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(ctx.DefaultQuery("size", "10"))
	category := ctx.Query("category")
	cacheKey := "articles:list:" + strconv.Itoa(page) + ":" + strconv.Itoa(size) + ":" + category

	cp := ensureCacheProtector()

	// 使用互斥锁防护：缓存过期时只有一个请求查DB，其余等待
	cached, err := cp.GetOrSetWithLock(
		ctx,
		cacheKey,
		5*time.Second, // 锁 TTL
		3*time.Second, // 最大等待时间
		func() (any, error) {
			offset := (page - 1) * size
			var articles []models.Article
			var total int64

			query := global.Db.Model(&models.Article{}).Where("status = ?", "published")
			if category != "" {
				query = query.Where("category = ?", category)
			}
			query.Count(&total)

			global.Db.Where("status = ?", "published").
				Preload("Author").
				Order("created_at DESC").
				Offset(offset).Limit(size).
				Find(&articles)

			return gin.H{
				"items": articles,
				"total": total,
				"page":  page,
				"size":  size,
			}, nil
		},
		5*time.Minute, // 缓存 TTL（内部会自动加随机抖动防雪崩）
	)

	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var result struct {
		Items []models.Article `json:"items"`
		Total int64            `json:"total"`
		Page  int              `json:"page"`
		Size  int              `json:"size"`
	}
	json.Unmarshal(cached, &result)
	ctx.JSON(http.StatusOK, result)
}

// ==================== GetArticleByID: 空值缓存防穿透 ====================
func GetArticleByID(ctx *gin.Context) {
	id := ctx.Param("id")
	cacheKey := "article:detail:" + id
	cp := ensureCacheProtector()

	// 使用空值缓存防护：不存在的ID也缓存，避免恶意请求穿透到DB
	cached, isNull, err := cp.GetOrSetWithNullGuard(
		ctx,
		cacheKey,
		30*time.Second, // 空值缓存时间（较短）
		func() (any, error) {
			var article models.Article
			if err := global.Db.Preload("Author").First(&article, id).Error; err != nil {
				// 返回 nil 表示数据不存在，会被缓存为空值标记
				return nil, nil
			}
			return article, nil
		},
		10*time.Minute, // 正常缓存时间
	)

	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if isNull {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "Article not found"})
		return
	}

	var article models.Article
	json.Unmarshal(cached, &article)

	// 异步增加浏览量（不阻塞响应）
	go func() {
		global.Db.Model(&models.Article{}).Where("id = ?", id).
			UpdateColumn("view_count", gorm.Expr("view_count + 1"))
		dateKey := "dau:" + time.Now().Format("2006-01-02")
		global.RedisDB.SetBit(ctx, dateKey, int64(article.AuthorID), 1)
		global.RedisDB.Expire(ctx, dateKey, 48*time.Hour)
	}()

	// 点赞数优先从 Redis 取
	likeKey := "article:" + id + ":likes"
	likes, _ := global.RedisDB.Get(ctx, likeKey).Int64()
	article.LikeCount = likes

	ctx.JSON(http.StatusOK, article)
}

func UpdateArticle(ctx *gin.Context) {
	userID := ctx.GetUint("userID")
	id := ctx.Param("id")

	var article models.Article
	if err := global.Db.First(&article, id).Error; err != nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "Article not found"})
		return
	}
	if article.AuthorID != userID {
		ctx.JSON(http.StatusForbidden, gin.H{"error": "not your article"})
		return
	}

	var input struct {
		Title    string `json:"title"`
		Content  string `json:"content"`
		Preview  string `json:"preview"`
		Status   string `json:"status"`
		Category string `json:"category"`
	}
	if err := ctx.ShouldBindJSON(&input); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updates := map[string]any{}
	if input.Title != "" {
		updates["title"] = sensitiveFilter.Replace(input.Title)
	}
	if input.Content != "" {
		updates["content"] = sensitiveFilter.Replace(input.Content)
	}
	if input.Preview != "" {
		updates["preview"] = sensitiveFilter.Replace(input.Preview)
	}
	if input.Status != "" {
		updates["status"] = input.Status
	}
	if input.Category != "" {
		updates["category"] = input.Category
	}

	if err := global.Db.Model(&article).Updates(updates).Error; err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 清除相关缓存
	global.RedisDB.Del(ctx, "article:detail:"+id)
	keys, _ := global.RedisDB.Keys(ctx, "articles:list:*").Result()
	if len(keys) > 0 {
		global.RedisDB.Del(ctx, keys...)
	}
	ctx.JSON(http.StatusOK, gin.H{"message": "article updated"})
}

func DeleteArticle(ctx *gin.Context) {
	userID := ctx.GetUint("userID")
	id := ctx.Param("id")

	var article models.Article
	if err := global.Db.First(&article, id).Error; err != nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "Article not found"})
		return
	}
	if article.AuthorID != userID {
		ctx.JSON(http.StatusForbidden, gin.H{"error": "not your article"})
		return
	}

	global.Db.Delete(&article)
	global.RedisDB.Del(ctx, "article:detail:"+id, "article:"+id+":likes")
	keys, _ := global.RedisDB.Keys(ctx, "articles:list:*").Result()
	if len(keys) > 0 {
		global.RedisDB.Del(ctx, keys...)
	}
	ctx.JSON(http.StatusOK, gin.H{"message": "article deleted"})
}

func SearchArticles(ctx *gin.Context) {
	q := ctx.Query("q")
	if q == "" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "query required"})
		return
	}

	page, _ := strconv.Atoi(ctx.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(ctx.DefaultQuery("size", "10"))
	offset := (page - 1) * size

	var articles []models.Article
	var total int64

	global.Db.Model(&models.Article{}).
		Where("MATCH(title, content) AGAINST(? IN BOOLEAN MODE) AND status = ?", "+"+q+"*", "published").
		Count(&total)

	global.Db.Where("MATCH(title, content) AGAINST(? IN BOOLEAN MODE) AND status = ?", "+"+q+"*", "published").
		Preload("Author").
		Order("created_at DESC").
		Offset(offset).Limit(size).
		Find(&articles)

	ctx.JSON(http.StatusOK, gin.H{
		"items": articles,
		"total": total,
		"page":  page,
		"size":  size,
	})
}
