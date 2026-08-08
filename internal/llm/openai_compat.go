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

// compatProvider 走 OpenAI-compatible 的 /chat/completions。
// Ollama（地端）與 Gemini（官方相容端點）共用這條路徑——差別只在 baseURL 與要不要帶 key。
type compatProvider struct {
	providerName string
	baseURL      string // 含到 /v1（或 Gemini 的 /v1beta/openai）
	apiKey       string // 空 = 不帶（Ollama）
	model        string
	http         *http.Client
	hint         string // 連線失敗時的提示
}

func newOllama(baseURL, model string) *compatProvider {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if model == "" {
		model = "qwen2.5:7b"
	}
	return &compatProvider{
		providerName: "ollama",
		baseURL:      baseURL + "/v1",
		model:        model,
		http:         &http.Client{Timeout: 300 * time.Second}, // 地端推論慢，給寬
		hint:         "（Ollama 有起來嗎？host: `ollama serve`，或 docker compose --profile local-llm up -d）",
	}
}

func newGemini(apiKey, model string) *compatProvider {
	if model == "" {
		model = "gemini-2.5-flash"
	}
	return &compatProvider{
		providerName: "gemini",
		baseURL:      "https://generativelanguage.googleapis.com/v1beta/openai",
		apiKey:       apiKey,
		model:        model,
		http:         &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *compatProvider) Name() (string, string) { return p.providerName, p.model }

type oaiReq struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens,omitempty"`
	Messages  []oaiMessage `json:"messages"`
}

type oaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaiResp struct {
	Choices []struct {
		Message oaiMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *compatProvider) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	msgs := []oaiMessage{}
	if req.SystemPrompt != "" {
		msgs = append(msgs, oaiMessage{Role: "system", Content: req.SystemPrompt})
	}
	msgs = append(msgs, oaiMessage{Role: "user", Content: req.UserPrompt})
	body, err := json.Marshal(oaiReq{Model: p.model, MaxTokens: req.MaxTokens, Messages: msgs})
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s: %w%s", p.providerName, err, p.hint)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var parsed oaiResp
	_ = json.Unmarshal(respBody, &parsed) // 先試著解析，錯誤訊息比裸 body 好讀
	if resp.StatusCode != http.StatusOK {
		msg := string(respBody)
		if parsed.Error != nil {
			msg = parsed.Error.Message
		}
		return nil, fmt.Errorf("%s: status %d: %s", p.providerName, resp.StatusCode, msg)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("%s: 空回應", p.providerName)
	}
	return &GenerateResponse{Text: parsed.Choices[0].Message.Content}, nil
}
