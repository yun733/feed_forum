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

func CreateComment(ctx *gin.Context) {
	userID := ctx.GetUint("userID")
	articleIDStr := ctx.Param("id")
	articleID, err := strconv.ParseUint(articleIDStr, 10, 64)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid article id"})
		return
	}

	var input struct {
		Content  string `json:"content" binding:"required"`
		ParentID *uint  `json:"parent_id"`
	}
	if err := ctx.ShouldBindJSON(&input); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	comment := models.Comment{
		Content:   input.Content,
		UserID:    userID,
		ArticleID: uint(articleID),
		ParentID:  input.ParentID,
	}

	if err := global.Db.Create(&comment).Error; err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 更新文章评论计数
	global.Db.Model(&models.Article{}).Where("id = ?", articleID).
		UpdateColumn("comment_count", global.Db.Raw("comment_count + 1"))

	// 预加载用户信息
	global.Db.Preload("User").First(&comment, comment.ID)

	// 异步发送通知
	go sendCommentNotification(comment)

	ctx.JSON(http.StatusOK, comment)
}

func sendCommentNotification(comment models.Comment) {
	var article models.Article
	global.Db.Select("author_id", "title").First(&article, comment.ArticleID)

	notif := map[string]interface{}{
		"user_id":      article.AuthorID,
		"from_user_id": comment.UserID,
		"type":         "comment",
		"target_id":    comment.ArticleID,
		"content":      comment.Content,
	}
	body, _ := json.Marshal(notif)
	mq.Publish("notification.comment", body)
}

func GetComments(ctx *gin.Context) {
	articleID := ctx.Param("id")

	var comments []models.Comment
	global.Db.Where("article_id = ?", articleID).
		Preload("User").
		Order("created_at ASC").
		Find(&comments)

	// 构建嵌套评论树
	commentTree := buildCommentTree(comments)

	ctx.JSON(http.StatusOK, gin.H{"comments": commentTree, "total": len(comments)})
}

func buildCommentTree(comments []models.Comment) []gin.H {
	// 分离一级评论和回复
	commentMap := make(map[uint][]models.Comment)
	var roots []models.Comment

	for _, c := range comments {
		if c.ParentID == nil {
			roots = append(roots, c)
		} else {
			commentMap[*c.ParentID] = append(commentMap[*c.ParentID], c)
		}
	}

	var build func(parent models.Comment) gin.H
	build = func(c models.Comment) gin.H {
		node := gin.H{
			"id":         c.ID,
			"content":    c.Content,
			"user":       c.User,
			"article_id": c.ArticleID,
			"like_count": c.LikeCount,
			"created_at": c.CreatedAt,
			"replies":    []gin.H{},
		}
		if children, ok := commentMap[c.ID]; ok {
			replies := make([]gin.H, 0, len(children))
			for _, child := range children {
				replies = append(replies, build(child))
			}
			node["replies"] = replies
		}
		return node
	}

	result := make([]gin.H, 0, len(roots))
	for _, root := range roots {
		result = append(result, build(root))
	}
	return result
}

func DeleteComment(ctx *gin.Context) {
	userID := ctx.GetUint("userID")
	id := ctx.Param("id")

	var comment models.Comment
	if err := global.Db.First(&comment, id).Error; err != nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "comment not found"})
		return
	}
	if comment.UserID != userID {
		ctx.JSON(http.StatusForbidden, gin.H{"error": "not your comment"})
		return
	}

	global.Db.Delete(&comment)
	global.Db.Model(&models.Article{}).Where("id = ?", comment.ArticleID).
		UpdateColumn("comment_count", global.Db.Raw("GREATEST(comment_count - 1, 0)"))
	ctx.JSON(http.StatusOK, gin.H{"message": "comment deleted"})
}
