package config

import (
	"log"

	"github.com/spf13/viper"
)

type Config struct {
	App struct {
		Name      string
		Port      string
		JWTSecret string
	}
	Database struct {
		Dsn          string
		MaxIdleConns int
		MaxOpenConns int
	}
	Redis struct {
		Addr     string
		Password string
		DB       int
	}
	RabbitMQ struct {
		URL string
	}
	Feed struct {
		// ===== 路由阈值：按粉丝量的 ABC 分层（写之前决策，不依赖运行时耗时）=====
		// A 类 < TierAMaxFollowers         同步推
		// B 类 TierAMaxFollowers~TierCMin  异步延迟推
		// C 类 >= TierCMinFollowers        不推，只写作者 outbox
		TierAMaxFollowers int `mapstructure:"tier_a_max_followers"`
		TierCMinFollowers int `mapstructure:"tier_c_min_followers"`

		// ===== 阈值反推依据（§4.6）=====
		// 可物化 inbox 上限 = InboxMemoryBudgetMB*1024*1024 / TimelineBytesPerUser
		InboxMemoryBudgetMB  int `mapstructure:"inbox_memory_budget_mb"`
		TimelineBytesPerUser int `mapstructure:"timeline_bytes_per_user"`
		// 日活用户数，用于把「可物化 inbox 上限」换算成阈值
		DauEstimate int `mapstructure:"dau_estimate"`

		TimelineMaxItems    int `mapstructure:"timeline_max_items"`
		TimelineTTLDays     int `mapstructure:"timeline_ttl_days"`
		ColdTimelineTTLDays int `mapstructure:"cold_timeline_ttl_days"`
		FanoutBatchSize     int `mapstructure:"fanout_batch_size"`
		// FanoutTimeoutMs 现在是纯保护机制（防单条消息永久阻塞消费者），
		// 不再充当降级触发器 —— 命中即告警，不再静默换策略（§4.2）
		FanoutTimeoutMs int `mapstructure:"fanout_timeout_ms"`
		CursorPageSize  int `mapstructure:"cursor_page_size"`

		// ===== C 类作者 outbox（§4.4）=====
		OutboxTTLDays  int `mapstructure:"outbox_ttl_days"`
		OutboxMaxItems int `mapstructure:"outbox_max_items"`
		// OutboxFetchLimit 每个大V单次读归并取多少条（控制 K 路归并的分母）
		OutboxFetchLimit int `mapstructure:"outbox_fetch_limit"`
		// MaxTierCAuthorsPerUser K 的硬上限（§8：一个用户关注的大V数有天然上限，超限按粉丝量截断）
		MaxTierCAuthorsPerUser int `mapstructure:"max_tier_c_authors_per_user"`
		// TierCSetCacheTTLSeconds 「我关注的大V集合」本地缓存 TTL（变更频率极低）
		TierCSetCacheTTLSeconds int `mapstructure:"tier_c_set_cache_ttl_seconds"`

		// ===== 维度二：只为活跃读者物化 inbox（§4.3）=====
		// MaterializeActiveOnly 为 true 时，fanout 只写活跃粉丝的 timeline
		MaterializeActiveOnly bool `mapstructure:"materialize_active_only"`

		// ===== 消费者背压（P7）=====
		ConsumerPrefetch int `mapstructure:"consumer_prefetch"`

		// ===== 失败重试（替代原来的 feed:degraded 死代码标记，P5）=====
		FanoutMaxRetries  int `mapstructure:"fanout_max_retries"`
		FanoutRetryScanMs int `mapstructure:"fanout_retry_scan_ms"`
	}
}

var AppConfig *Config

func InitConfig() {
	viper.SetConfigName("config")
	viper.SetConfigType("yml")
	viper.AddConfigPath("./config/")
	if err := viper.ReadInConfig(); err != nil {
		log.Fatalf("Error reading config file: %v", err)
	}
	AppConfig = &Config{}
	if err := viper.Unmarshal(AppConfig); err != nil {
		log.Fatalf("Error unmarshalling config: %v", err)
	}
}
