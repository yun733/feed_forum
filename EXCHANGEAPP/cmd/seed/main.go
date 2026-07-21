package main

import (
	"context"
	"exchangeapp/config"
	"exchangeapp/global"
	"exchangeapp/models"
	"exchangeapp/utils"
	"fmt"
	"log"
	"math/rand"
	"time"
)

func main() {
	config.InitConfig()
	config.InitDB()
	config.InitRedis()

	db := global.Db
	rdb := global.RedisDB
	rand.Seed(time.Now().UnixNano())

	fmt.Println("=== 开始造数据 ===")

	// ====================== 1. 创建用户 ======================
	userCount := 50
	users := make([]models.User, userCount)
	for i := 0; i < userCount; i++ {
		username := fmt.Sprintf("testuser_%d", i+1)
		hashedPwd, _ := utils.HashPassword("123456")
		users[i] = models.User{
			Username:     username,
			Password:     hashedPwd,
			Bio:          fmt.Sprintf("这是 %s 的个人简介", username),
			LastActiveAt: time.Now(),
		}
	}
	if err := db.Create(&users).Error; err != nil {
		log.Printf("创建用户跳过（可能已存在）: %v", err)
		// 尝试逐个创建
		for i := range users {
			db.Where("username = ?", users[i].Username).FirstOrCreate(&users[i])
		}
	}
	fmt.Printf("✅ 创建了 %d 个测试用户\n", userCount)

	// 获取 lynn 用户（通常是你的测试账号）
	var lynnUser models.User
	db.Where("username = ?", "lynn").First(&lynnUser)
	if lynnUser.ID == 0 {
		log.Fatal("找不到用户 lynn，请先注册")
	}
	fmt.Printf("   lynn 的用户 ID: %d\n", lynnUser.ID)

	// ====================== 2. 创建文章 ======================
	categories := []string{"tech", "life", "sports", "entertainment", "science", "food"}
	titles := []string{
		"Golang性能优化实战", "Redis缓存设计模式", "Docker容器化部署指南",
		"MySQL索引优化技巧", "微服务架构设计思路", "Kubernetes入门教程",
		"前端Vue3响应式原理", "TypeScript高级类型", "Python数据分析",
		"分布式系统一致性", "消息队列选型指南", "数据库分库分表",
		"机器学习入门", "Linux常用命令", "Git工作流最佳实践",
		"RESTful API设计", "GraphQL初探", "WebSocket实时通信",
		"单元测试编写技巧", "持续集成CI/CD", "代码审查要点",
		"安全漏洞防范", "云原生架构", "Serverless实践",
		"最近读了什么书", "周末去哪里玩", "推荐一部好电影",
		"健身减脂经验", "今日美食分享", "旅行见闻",
		"科技新闻解读", "编程学习路线", "面试经验总结",
		"工作中的趣事", "摄影技巧", "养花心得",
	}

	articleCount := 500
	articles := make([]models.Article, articleCount)
	for i := 0; i < articleCount; i++ {
		author := users[rand.Intn(userCount)]
		title := titles[rand.Intn(len(titles))]
		cat := categories[rand.Intn(len(categories))]
		status := "published"
		if rand.Float64() < 0.05 {
			status = "draft" // 5% 草稿
		}

		articles[i] = models.Article{
			Title:    fmt.Sprintf("%s (%d)", title, i+1),
			Content:  generateContent(i),
			Preview:  fmt.Sprintf("这是第 %d 篇文章的摘要预览...", i+1),
			AuthorID: author.ID,
			Status:   status,
			Category: cat,
			ViewCount: int64(rand.Intn(10000)),
		}
	}
	if err := db.Create(&articles).Error; err != nil {
		log.Fatalf("创建文章失败: %v", err)
	}
	fmt.Printf("✅ 创建了 %d 篇文章\n", articleCount)

	// ====================== 3. 创建关注关系 ======================
	// lynn 关注前 30 个测试用户
	var follows []models.Follow
	for i := 0; i < 30; i++ {
		follows = append(follows, models.Follow{
			FollowerID:  lynnUser.ID,
			FollowingID: users[i].ID,
		})
	}
	// 用户之间互相随机关注（100 对）
	for i := 0; i < 100; i++ {
		follower := users[rand.Intn(userCount)]
		following := users[rand.Intn(userCount)]
		if follower.ID == following.ID {
			continue
		}
		follows = append(follows, models.Follow{
			FollowerID:  follower.ID,
			FollowingID: following.ID,
		})
	}
	// 批量插入，忽略重复
	for _, f := range follows {
		db.Where("follower_id = ? AND following_id = ?", f.FollowerID, f.FollowingID).
			FirstOrCreate(&f)
	}
	fmt.Printf("✅ 创建了关注关系（lynn 关注了 30 人 + 随机关注对）\n")

	// ====================== 4. 设置活跃用户（写入 Redis）======================
	ctx := context.Background()
	pipe := rdb.Pipeline()
	for i := 0; i < 35; i++ {
		key := fmt.Sprintf("user:active:%d", users[i].ID)
		pipe.Set(ctx, key, "1", 7*24*time.Hour)
	}
	// lynn 自己也是活跃的
	pipe.Set(ctx, fmt.Sprintf("user:active:%d", lynnUser.ID), "1", 7*24*time.Hour)
	pipe.Exec(ctx)
	fmt.Printf("✅ 设置了 35 个活跃用户（Redis user:active:*）\n")

	// ====================== 5. 统计 ======================
	var totalUsers, totalArticles, totalFollows int64
	db.Model(&models.User{}).Count(&totalUsers)
	db.Model(&models.Article{}).Where("status = ?", "published").Count(&totalArticles)
	db.Model(&models.Follow{}).Count(&totalFollows)

	fmt.Println()
	fmt.Println("=== 数据统计 ===")
	fmt.Printf("  总用户数: %d\n", totalUsers)
	fmt.Printf("  已发布文章: %d\n", totalArticles)
	fmt.Printf("  关注关系: %d\n", totalFollows)
	fmt.Println("=== 完成！可以开始压测了 ===")
}

func generateContent(i int) string {
	paragraphs := []string{
		"这是一段关于技术的深入讨论。在现代软件开发中，选择合适的架构模式至关重要。微服务架构虽然灵活，但也带来了运维复杂度。",
		"最近在工作中遇到了一个有趣的挑战。当数据量增长到一定程度时，传统的查询方式已经无法满足性能需求，需要引入缓存层。",
		"今天想和大家分享一些心得体会。在学习新技术的道路上，我们经常会遇到瓶颈，但只要坚持下去，总会有突破的那一天。",
		"这个话题我一直想写很久了。从最初的构想到最终实现，经历了无数次的修改和优化，最终呈现给大家的是一个相对完善的方案。",
		"周末去参加了一个技术交流会，收获很多。现场有来自各大公司的工程师分享了他们在实际项目中的经验和教训，非常有启发。",
	}
	p := paragraphs[i%len(paragraphs)]
	return fmt.Sprintf("%s\n\n这是第 %d 篇文章的正文内容。\n\n%s", p, i+1, p)
}
