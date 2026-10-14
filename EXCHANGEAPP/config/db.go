package config

import (
	"exchangeapp/global"
	"exchangeapp/models"
	"log"
	"strings"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func InitDB() {
	var err error
	dsn := AppConfig.Database.Dsn
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("failed to initialize database, get error: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatalf("failed to get generic database object: %v", err)
	}
	sqlDB.SetMaxOpenConns(AppConfig.Database.MaxOpenConns)
	sqlDB.SetMaxIdleConns(AppConfig.Database.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(time.Hour)

	if err := db.AutoMigrate(
		&models.User{},
		&models.Article{},
		&models.Comment{},
		&models.Follow{},
		&models.Like{},
		&models.Notification{},
		&models.ExchangeRate{},
		&models.OutboxMessage{}, // 新增：本地消息表
	); err != nil {
		log.Fatalf("failed to auto migrate: %v", err)
	}

	// MySQL 8.0 全文索引（手动创建，GORM AutoMigrate 不支持）
	// 兼容旧版 MySQL：IF NOT EXISTS 需 8.0.29+，这里用 error 1065 兜底
	if err := db.Exec("ALTER TABLE articles ADD FULLTEXT INDEX ft_article_search (title, content)").Error; err != nil {
		if isDuplicateIndexError(err) {
			log.Println("[DB] Fulltext index ft_article_search already exists, skipped")
		} else {
			log.Printf("[DB] Failed to create fulltext index ft_article_search: %v", err)
		}
	}

	// ========== 新增索引：Feed 流性能优化 ==========
	createIndexSilently(db, "idx_outbox_status_next_retry", "outbox_messages", "status, next_retry_at")
	createIndexSilently(db, "idx_articles_feed_scan", "articles", "status, created_at DESC")
	createIndexSilently(db, "idx_articles_author_time", "articles", "author_id, status, created_at DESC")
	createIndexSilently(db, "idx_follows_follower", "follows", "follower_id")
	createIndexSilently(db, "idx_follows_following", "follows", "following_id")
	// fanout keyset pagination 专用：WHERE following_id = ? AND deleted_at IS NULL
	// ORDER BY follower_id LIMIT n
	//
	// 三列缺一不可，原因在压测里被抓出来了：
	// GORM 的软删除会给查询补上 `deleted_at IS NULL`，而 follows 上的
	// `idx_follows_deleted_at`（gorm.Model 自动建的）会让优化器误判 ——
	// 它选了那个索引，然后对 4.8 万行做 **filesort**，单批 1000 行要 507ms，
	// 比旧代码一次性 Pluck 10 万行（407ms）还慢。
	//
	// 把 deleted_at 并进索引后，这个查询变成 `Using index`（索引内有序 + 索引内过滤），
	// 不需要任何 FORCE INDEX 提示，也不依赖优化器的估算准不准。
	createIndexSilently(db, "idx_follows_fanout", "follows", "following_id, follower_id, deleted_at")
	createIndexSilently(db, "idx_users_active", "users", "last_active_at")

	log.Println("[DB] All indexes verified/created")

	global.Db = db
}

// createIndexSilently 创建索引，兼容旧版 MySQL（不支持 IF NOT EXISTS）
// MySQL 8.0.29+ 才支持 CREATE INDEX IF NOT EXISTS，旧版需忽略 1061 重复错误
func createIndexSilently(db *gorm.DB, indexName, tableName, columns string) {
	sql := "CREATE INDEX " + indexName + " ON " + tableName + " (" + columns + ")"
	if err := db.Exec(sql).Error; err != nil {
		if isDuplicateIndexError(err) {
			log.Printf("[DB] Index %s already exists, skipped", indexName)
		} else {
			log.Printf("[DB] Failed to create index %s: %v", indexName, err)
		}
	}
}

// isDuplicateIndexError 判断是否为索引重复错误 (MySQL 1061/1065)
func isDuplicateIndexError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1061") || strings.Contains(msg, "Error 1065") ||
		strings.Contains(msg, "Duplicate key name") || strings.Contains(msg, "Duplicate index")
}
