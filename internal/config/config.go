// Package config 從環境變數（+ 選配 .env 檔）載入全案設定。
package config

import (
	"os"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	DatabaseURL string

	RedisAddr     string // 空 = 停用（lock / rate limit 變 no-op）
	RedisPassword string

	LLMProvider     string
	AnthropicAPIKey string
	AnthropicModel  string
	GeminiAPIKey    string
	GeminiModel     string
	OllamaURL       string
	OllamaModel     string

	NewsFeeds []string

	// DiscordWebhookURL 空 = 不推播（push 指令會明確報錯，不靜默跳過）。
	DiscordWebhookURL string

	EnableKafka  bool // 預留（Phase 3 事件管線），Phase 1 不使用
	KafkaBrokers string
}

// Load 讀 .env（存在才讀，不存在不算錯）再讀環境變數。
func Load() Config {
	_ = godotenv.Load()
	feeds := strings.Split(getenv("NEWS_FEEDS", "https://news.ltn.com.tw/rss/business.xml"), ",")
	for i := range feeds {
		feeds[i] = strings.TrimSpace(feeds[i])
	}
	return Config{
		DatabaseURL:     getenv("DATABASE_URL", "postgres://stock:devpass_change_me@localhost:5432/stock?sslmode=disable"),
		RedisAddr:       os.Getenv("REDIS_ADDR"),
		RedisPassword:   os.Getenv("REDIS_PASSWORD"),
		LLMProvider:     getenv("LLM_PROVIDER", "anthropic"),
		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"),
		AnthropicModel:  os.Getenv("ANTHROPIC_MODEL"),
		GeminiAPIKey:    os.Getenv("GEMINI_API_KEY"),
		GeminiModel:     os.Getenv("GEMINI_MODEL"),
		OllamaURL:       os.Getenv("OLLAMA_URL"),
		OllamaModel:     os.Getenv("OLLAMA_MODEL"),
		NewsFeeds:       feeds,

		DiscordWebhookURL: os.Getenv("DISCORD_WEBHOOK_URL"),
		EnableKafka:       os.Getenv("ENABLE_KAFKA") == "true",
		KafkaBrokers:      os.Getenv("KAFKA_BROKERS"),
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
