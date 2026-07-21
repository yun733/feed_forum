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
		BigVThreshold     int `mapstructure:"big_v_threshold"`
		TimelineMaxItems  int `mapstructure:"timeline_max_items"`
		TimelineTTLDays   int `mapstructure:"timeline_ttl_days"`
		ColdTimelineTTLDays int `mapstructure:"cold_timeline_ttl_days"`
		FanoutBatchSize   int `mapstructure:"fanout_batch_size"`
		FanoutTimeoutMs   int `mapstructure:"fanout_timeout_ms"`
		CursorPageSize    int `mapstructure:"cursor_page_size"`
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
