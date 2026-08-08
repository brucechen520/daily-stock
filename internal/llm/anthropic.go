package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// anthropicProvider 直接打 Messages API，不拉 SDK（Phase 1 只要單輪文字）。
type anthropicProvider struct {
	apiKey  string
	model   string
	baseURL string
	http    *http.Client
}

func newAnthropic(apiKey, model string) *anthropicProvider {
	if model == "" {
		model = "claude-sonnet-5"
	}
	return &anthropicProvider{
		apiKey:  apiKey,
		model:   model,
		baseURL: "https://api.anthropic.com",
		http:    &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *anthropicProvider) Name() (string, string) { return "anthropic", p.model }

type anthropicReq struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResp struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (p *anthropicProvider) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 1024
	}
	body, err := json.Marshal(anthropicReq{
		Model:     p.model,
		MaxTokens: maxTokens,
		System:    req.SystemPrompt,
		Messages:  []anthropicMessage{{Role: "user", Content: req.UserPrompt}},
	})
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var parsed anthropicResp
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("anthropic: 解析回應失敗 (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := string(respBody)
		if parsed.Error != nil {
			msg = parsed.Error.Message
		}
		return nil, fmt.Errorf("anthropic: status %d: %s", resp.StatusCode, msg)
	}
	var text string
	for _, c := range parsed.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	return &GenerateResponse{Text: text}, nil
}
