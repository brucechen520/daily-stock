package report_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brucechen520/daily-stock/internal/llm"
	"github.com/brucechen520/daily-stock/internal/report"
)

func snap() report.Snapshot {
	return report.Snapshot{
		Date:        time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC),
		IndexClose:  44396.70,
		IndexChange: -214.90,
		ForeignNet:  2_019_818_140,  // 買超 20.2 億
		TrustNet:    9_408_386_624,  // 買超 94.1 億
		DealerNet:   -6_312_818_587, // 賣超 63.1 億
	}
}

func TestFields_FormatsDirectionAndUnits(t *testing.T) {
	f := snap().Fields()

	if want := "44,396.70"; f["index_close"] != want {
		t.Errorf("index_close = %q, want %q（千分位）", f["index_close"], want)
	}
	if want := "下跌 214.90 點（-0.48%）"; f["index_change_desc"] != want {
		t.Errorf("index_change_desc = %q, want %q（方向詞由程式給）", f["index_change_desc"], want)
	}
	if want := "買超 20.2 億元"; f["foreign_net_desc"] != want {
		t.Errorf("foreign_net_desc = %q, want %q", f["foreign_net_desc"], want)
	}
	if want := "賣超 63.1 億元"; f["dealer_net_desc"] != want {
		t.Errorf("dealer_net_desc = %q, want %q（負值 → 賣超 + 絕對值）", f["dealer_net_desc"], want)
	}
}

func TestValidate_AcceptsPlaceholderOnlyText(t *testing.T) {
	text := "加權指數收 {index_close} 點，{index_change_desc}。外資{foreign_net_desc}。"

	err := report.Validate(text, snap().Fields())

	if err != nil {
		t.Errorf("Validate(合法文本) = %v, want nil", err)
	}
}

func TestValidate_RejectsUnknownPlaceholder(t *testing.T) {
	err := report.Validate("外資 {foreign_total} 進場", snap().Fields())

	if err == nil {
		t.Error("Validate(未知佔位符) = nil, want 錯誤")
	}
}

func TestValidate_RejectsLiteralDigits(t *testing.T) {
	// LLM 自己抄了數字（防幻覺紅線）
	err := report.Validate("加權指數收 44396 點，外資{foreign_net_desc}", snap().Fields())

	if err == nil {
		t.Error("Validate(佔位符外有數字) = nil, want 錯誤")
	}
}

func TestValidate_RejectsFullWidthDigits(t *testing.T) {
	err := report.Validate("大盤上漲２００點", snap().Fields())

	if err == nil {
		t.Error("Validate(全形數字) = nil, want 錯誤")
	}
}

func TestRender_SubstitutesRealValues(t *testing.T) {
	got := report.Render("收 {index_close} 點，外資{foreign_net_desc}", snap().Fields())

	if want := "收 44,396.70 點，外資買超 20.2 億元"; got != want {
		t.Errorf("Render = %q, want %q", got, want)
	}
}

// fakeProvider 依序回放預設回應。
type fakeProvider struct {
	responses []string
	calls     int
	prompts   []llm.GenerateRequest
}

func (f *fakeProvider) Generate(_ context.Context, req llm.GenerateRequest) (*llm.GenerateResponse, error) {
	f.prompts = append(f.prompts, req)
	if f.calls >= len(f.responses) {
		return nil, errors.New("fake: 沒有更多回應")
	}
	r := f.responses[f.calls]
	f.calls++
	return &llm.GenerateResponse{Text: r}, nil
}

func (f *fakeProvider) Name() (string, string) { return "fake", "fake-1" }

const goodLLMText = "加權指數收 {index_close} 點，{index_change_desc}，屬於溫和整理格局。\n\n外資{foreign_net_desc}，投信{trust_net_desc}，自營商{dealer_net_desc}，法人看法分歧。"

func TestGenerate_ProducesRenderedSummaryWithGlossaryAndDisclaimer(t *testing.T) {
	p := &fakeProvider{responses: []string{goodLLMText}}

	out, err := report.Generate(context.Background(), p, snap())

	if err != nil {
		t.Fatalf("Generate 回傳非預期錯誤: %v", err)
	}
	for _, want := range []string{
		"44,396.70",      // 真值已代入
		"買超 20.2 億元", // 外資
		"【術語小教室】", // deterministic 附加
		"非投資建議",     // 免責
		"2026-08-06",     // 日期
	} {
		if !strings.Contains(out, want) {
			t.Errorf("輸出缺少 %q。完整輸出：\n%s", want, out)
		}
	}
	if strings.Contains(out, "{") {
		t.Errorf("輸出殘留未代入的佔位符：\n%s", out)
	}
}

// 第一次違規（自己寫數字）→ 錯誤回饋重試 → 第二次合法 → 成功。
func TestGenerate_RetriesOnceOnViolation(t *testing.T) {
	p := &fakeProvider{responses: []string{"大盤收 44396 點", goodLLMText}}

	out, err := report.Generate(context.Background(), p, snap())

	if err != nil {
		t.Fatalf("Generate 回傳非預期錯誤: %v", err)
	}
	if p.calls != 2 {
		t.Errorf("LLM 被呼叫 %d 次, want 2（違規要重試一次）", p.calls)
	}
	if !strings.Contains(p.prompts[1].UserPrompt, "違反規則") {
		t.Error("重試 prompt 未包含違規原因回饋")
	}
	if !strings.Contains(out, "44,396.70") {
		t.Error("重試成功後輸出應含代入後的真值")
	}
}

func TestGenerate_FailsAfterSecondViolation(t *testing.T) {
	p := &fakeProvider{responses: []string{"收 44396 點", "還是 44396 點"}}

	_, err := report.Generate(context.Background(), p, snap())

	if err == nil {
		t.Error("連續兩次違規應回錯誤, got nil")
	}
}
