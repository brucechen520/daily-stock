package notify_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brucechen520/daily-stock/internal/notify"
)

// recorder 收下所有 webhook 請求的 content 欄位。
type recorder struct {
	bodies []string
	fail   int32 // 前 N 次回 500
	calls  int32
}

func (r *recorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&r.calls, 1)
		if atomic.LoadInt32(&r.fail) > 0 {
			atomic.AddInt32(&r.fail, -1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		b, _ := io.ReadAll(req.Body)
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(b, &payload); err != nil {
			t.Errorf("webhook body 不是合法 JSON: %v", err)
		}
		r.bodies = append(r.bodies, payload.Content)
		w.WriteHeader(http.StatusNoContent)
	}))
}

func newClient(url string) *notify.Discord {
	d := notify.NewDiscord(url)
	d.RetryWait = time.Millisecond // 測試不真的等
	return d
}

func TestDiscord_SendsOneMessagePerPackedChunk(t *testing.T) {
	rec := &recorder{}
	srv := rec.server(t)
	defer srv.Close()

	err := newClient(srv.URL).Send(context.Background(), []string{"第一則", "第二則"})

	if err != nil {
		t.Fatalf("Send 回傳非預期錯誤: %v", err)
	}
	if len(rec.bodies) != 2 {
		t.Fatalf("送出 %d 則, want 2", len(rec.bodies))
	}
	if rec.bodies[0] != "第一則" || rec.bodies[1] != "第二則" {
		t.Errorf("訊息內容或順序有誤: %v", rec.bodies)
	}
}

func TestDiscord_RetriesTransientFailure(t *testing.T) {
	rec := &recorder{fail: 2} // 前兩次 500，第三次成功
	srv := rec.server(t)
	defer srv.Close()

	err := newClient(srv.URL).Send(context.Background(), []string{"訊息"})

	if err != nil {
		t.Fatalf("重試後應成功, got %v", err)
	}
	if got := atomic.LoadInt32(&rec.calls); got != 3 {
		t.Errorf("呼叫 %d 次, want 3（失敗兩次後成功）", got)
	}
}

// 第二則掛掉時：不重送整批（會洗頻道），但要讓呼叫端知道推播不完整。
func TestDiscord_ReportsPartialFailureWithoutResendingSucceeded(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		if strings.Contains(string(b), "第二則") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	err := newClient(srv.URL).Send(context.Background(), []string{"第一則", "第二則", "第三則"})

	if err == nil {
		t.Fatal("部分失敗應回錯誤, got nil")
	}
	if !strings.Contains(err.Error(), "2") {
		t.Errorf("錯誤訊息應指出是第 2 則失敗: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("成功送出 %d 則, want 1（第一則不該重送，第三則不該續送）", got)
	}
}

func TestDiscord_NotConfiguredIsExplicitError(t *testing.T) {
	err := notify.NewDiscord("").Send(context.Background(), []string{"訊息"})

	if err == nil {
		t.Error("未設定 webhook 應回錯誤（靜默不送會讓人以為推播正常）")
	}
}

func TestDiscord_NothingToSendIsNotAnError(t *testing.T) {
	if err := notify.NewDiscord("").Send(context.Background(), nil); err != nil {
		t.Errorf("沒有訊息就不必送, got %v", err)
	}
}
