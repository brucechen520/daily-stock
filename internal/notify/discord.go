package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Discord 用 webhook 推播（純文字）。webhook 只需一個 URL，
// 不必 bot token / 權限 / 長駐連線——embed 與 bot 見 BACKLOG.md。
type Discord struct {
	WebhookURL string
	HTTP       *http.Client
	Retries    int
	RetryWait  time.Duration
}

func NewDiscord(webhookURL string) *Discord {
	return &Discord{
		WebhookURL: webhookURL,
		HTTP:       &http.Client{Timeout: 15 * time.Second},
		Retries:    3,
		RetryWait:  2 * time.Second,
	}
}

// Configured 回報是否可用（未設定 webhook 時呼叫端可提早給出可讀的提示）。
func (d *Discord) Configured() bool { return d.WebhookURL != "" }

// Send 依序送出每一則。
//
// 失敗語意（docs/phase-1.5.md §4.3）：某則重試耗盡就停止並回錯誤，
// 已送出的不重送——重複訊息洗頻道比缺一則更煩。呼叫端收到錯誤後
// 應補送一則「推播不完整」，而不是重跑整批。
func (d *Discord) Send(ctx context.Context, messages []string) error {
	if len(messages) == 0 {
		return nil
	}
	if !d.Configured() {
		return fmt.Errorf("notify: DISCORD_WEBHOOK_URL 未設定")
	}
	for i, m := range messages {
		if err := d.post(ctx, m); err != nil {
			return fmt.Errorf("notify: 第 %d/%d 則送出失敗（前 %d 則已送達，不重送）：%w",
				i+1, len(messages), i, err)
		}
	}
	return nil
}

func (d *Discord) post(ctx context.Context, content string) error {
	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt <= d.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d.RetryWait):
			}
		}
		lastErr = d.postOnce(ctx, body)
		if lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func (d *Discord) postOnce(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook 回應 %d", resp.StatusCode)
	}
	return nil
}
