// Package llm 是 LLM provider 抽象：Claude / Ollama config 一行切，上層零改動。
// Phase 3 會擴充 Messages + Tools；Phase 1 先單輪。
package llm

import (
	"context"
	"fmt"
)

type GenerateRequest struct {
	SystemPrompt string
	UserPrompt   string
	MaxTokens    int
}

type GenerateResponse struct {
	Text string
}

type Provider interface {
	Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error)
	// Name 回傳 (provider, model)，落 summaries 表用。
	Name() (provider, model string)
}

// Config 由環境變數組出（見 .env.example）。
type Config struct {
	Provider string // "anthropic" | "gemini" | "ollama"

	AnthropicAPIKey string
	AnthropicModel  string

	GeminiAPIKey string
	GeminiModel  string

	OllamaURL   string
	OllamaModel string
}

// New 依 config 建 provider。
func New(cfg Config) (Provider, error) {
	switch cfg.Provider {
	case "anthropic", "claude", "":
		if cfg.AnthropicAPIKey == "" {
			return nil, fmt.Errorf("llm: ANTHROPIC_API_KEY 未設（或改用 LLM_PROVIDER=gemini / ollama）")
		}
		return newAnthropic(cfg.AnthropicAPIKey, cfg.AnthropicModel), nil
	case "gemini":
		if cfg.GeminiAPIKey == "" {
			return nil, fmt.Errorf("llm: GEMINI_API_KEY 未設")
		}
		return newGemini(cfg.GeminiAPIKey, cfg.GeminiModel), nil
	case "ollama", "local":
		return newOllama(cfg.OllamaURL, cfg.OllamaModel), nil
	default:
		return nil, fmt.Errorf("llm: 未知 provider %q（anthropic | gemini | ollama）", cfg.Provider)
	}
}
