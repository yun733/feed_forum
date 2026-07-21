package router

import (
	"exchangeapp/controllers"
	"exchangeapp/middlewares"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

func SetupRouter() *gin.Engine {
	r := gin.Default()
	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"http://localhost:5173"},
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	// 全局限流：100 req/s, burst 200
	// 可选：r.Use(middlewares.TokenBucketRateLimiter(10, 20))

	// 公开路由
	auth := r.Group("/api/auth")
	{
		auth.POST("/login", controllers.Login)
		auth.POST("/register", controllers.Register)
	}

	api := r.Group("/api")
	api.GET("/exchangeRates", controllers.GetExchangeRates)

	// 需认证路由
	protected := api.Group("")
	protected.Use(middlewares.AuthMiddleware())
	{
		// 汇率
		protected.POST("/exchangeRates", controllers.CreateExchangeRate)

		// 文章
		protected.POST("/articles", controllers.CreateArticle)
		protected.GET("/articles", controllers.GetArticles)
		protected.GET("/articles/search", controllers.SearchArticles)
		protected.GET("/articles/:id", controllers.GetArticleByID)
		protected.PUT("/articles/:id", controllers.UpdateArticle)
		protected.DELETE("/articles/:id", controllers.DeleteArticle)

		// 点赞
		protected.POST("/articles/:id/like", controllers.LikeArticle)
		protected.GET("/articles/:id/like", controllers.GetArticleLikes)
		protected.GET("/articles/:id/like/status", controllers.CheckLikeStatus)
		protected.POST("/comments/:id/like", controllers.LikeComment)

		// 评论
		protected.POST("/articles/:id/comments", controllers.CreateComment)
		protected.GET("/articles/:id/comments", controllers.GetComments)
		protected.DELETE("/comments/:id", controllers.DeleteComment)

		// 关注
		protected.POST("/users/:id/follow", controllers.FollowUser)
		protected.GET("/users/:id/followers", controllers.GetFollowers)
		protected.GET("/users/:id/following", controllers.GetFollowing)
		protected.GET("/users/:id/follow/status", controllers.CheckFollowStatus)

		// Feed
		protected.GET("/feed", controllers.GetFeed)
		protected.GET("/feed/hot", controllers.GetHotFeed)

		// 用户
		protected.GET("/users/:id", controllers.GetUserProfile)
		protected.PUT("/users/profile", controllers.UpdateProfile)
		protected.GET("/users/:id/articles", controllers.GetUserArticles)

		// 通知
		protected.GET("/notifications", controllers.GetNotifications)
		protected.PUT("/notifications/:id/read", controllers.MarkNotificationRead)
		protected.GET("/notifications/unread-count", controllers.GetUnreadCount)
	}

	return r
}
