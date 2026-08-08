// Package report 產生每日白話摘要。防幻覺設計（docs/phase-1.md §3）：
//
//	LLM 只准輸出 {欄位} 佔位符與純文字（一個數字都不准寫）→
//	程式把真值代入佔位符 → 數字想錯都錯不了。
//
// 【術語小教室】與免責聲明由程式 deterministic 附加，不經 LLM。
package report

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/brucechen520/daily-stock/internal/glossary"
	"github.com/brucechen520/daily-stock/internal/llm"
)

// Snapshot 是當日摘要素材（全部來自 store 的真實資料）。
type Snapshot struct {
	Date        time.Time
	IndexClose  float64
	IndexChange float64
	ForeignNet  int64 // 元
	TrustNet    int64
	DealerNet   int64
}

// Fields 把 Snapshot 轉成佔位符 → 格式化真值。
// 方向詞（買超/賣超、上漲/下跌）也由程式給——連方向都不讓 LLM 說錯。
func (s Snapshot) Fields() map[string]string {
	prevClose := s.IndexClose - s.IndexChange
	pct := 0.0
	if prevClose != 0 {
		pct = s.IndexChange / prevClose * 100
	}
	return map[string]string{
		"date":              s.Date.Format("2006-01-02"),
		"index_close":       formatFloat(s.IndexClose),
		"index_change_desc": fmt.Sprintf("%s %s 點（%+.2f%%）", upDown(s.IndexChange), formatFloat(abs(s.IndexChange)), pct),
		"foreign_net_desc":  netDesc(s.ForeignNet),
		"trust_net_desc":    netDesc(s.TrustNet),
		"dealer_net_desc":   netDesc(s.DealerNet),
	}
}

func netDesc(net int64) string {
	side := "買超"
	if net < 0 {
		side = "賣超"
		net = -net
	}
	return fmt.Sprintf("%s %.1f 億元", side, float64(net)/1e8)
}

func upDown(v float64) string {
	switch {
	case v > 0:
		return "上漲"
	case v < 0:
		return "下跌"
	default:
		return "平盤，漲跌"
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// formatFloat 千分位 + 兩位小數（對齊 TWSE 慣例顯示）。
func formatFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	dot := strings.Index(s, ".")
	intPart, frac := s[:dot], s[dot:]
	neg := strings.HasPrefix(intPart, "-")
	intPart = strings.TrimPrefix(intPart, "-")
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := b.String() + frac
	if neg {
		out = "-" + out
	}
	return out
}

const systemPrompt = `你是台股市場的說明員，把真實數據解讀成白話文，幫助投資新手理解「今天發生了什麼、代表什麼現象」。

嚴格規則（違反任何一條即為失敗）：
1. 輸出中「不可出現任何阿拉伯數字」。所有數值一律用佔位符引用，格式 {欄位名}，只能用下方列出的欄位。
2. 不可給出買賣建議（禁止「建議買進」「應該賣出」「可進場」「加碼」「停損」等字眼），只能解釋現象。
3. 若參考數據區某項標示「無資料」，寫「今日無相關資料」，不可推測。
4. 語氣中性克制，「偏多訊號之一」而非「大漲在即」。
5. 產出 150-250 字，兩段：第一段大盤，第二段三大法人。不要標題、不要日期、不要免責聲明（程式會加）。

正確輸出範例（照這個格式，數值處全用佔位符）：
加權指數收 {index_close} 點，{index_change_desc}，顯示市場暫時處於整理格局，多空雙方尚未表態。

三大法人方面，外資{foreign_net_desc}，投信{trust_net_desc}，自營商{dealer_net_desc}。外資站在買方是短線偏多訊號之一，但各法人動向不一致，後續仍需觀察量能變化。`

// buildUserPrompt 給 LLM 看真實數據（供推理），但要求輸出只用佔位符。
func buildUserPrompt(s Snapshot) string {
	f := s.Fields()
	var b strings.Builder
	b.WriteString("可用佔位符（輸出時引用數值只能用這些）：\n")
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  {%s} = %s\n", k, f[k])
	}
	b.WriteString("\n參考數據（幫助你判斷方向與力度，輸出中不可直接寫出這些數字）：\n")
	fmt.Fprintf(&b, "  加權指數收 {index_close}，%s\n", f["index_change_desc"])
	fmt.Fprintf(&b, "  外資%s、投信%s、自營商%s\n", f["foreign_net_desc"], f["trust_net_desc"], f["dealer_net_desc"])
	b.WriteString("\n請生成白話摘要。範例句式：「加權指數收 {index_close} 點，{index_change_desc}…」")
	return b.String()
}

var (
	placeholderRe = regexp.MustCompile(`\{([a-z_]+)\}`)
	digitRe       = regexp.MustCompile(`[0-9０-９]`)
)

// Validate 檢查 LLM 原始輸出（代入前）是否守規則。
// 回傳的錯誤訊息會餵回給 LLM 重試一次。
func Validate(text string, allowed map[string]string) error {
	// 1. 佔位符必須全部合法
	for _, m := range placeholderRe.FindAllStringSubmatch(text, -1) {
		if _, ok := allowed[m[1]]; !ok {
			return fmt.Errorf("使用了不存在的佔位符 {%s}", m[1])
		}
	}
	// 2. 佔位符以外不准出現任何數字——把佔位符挖掉後全文掃數字
	stripped := placeholderRe.ReplaceAllString(text, "")
	if loc := digitRe.FindStringIndex(stripped); loc != nil {
		start := max(0, loc[0]-15)
		end := min(len(stripped), loc[1]+15)
		return fmt.Errorf("佔位符之外出現了數字（所有數值必須用佔位符），出現在「…%s…」",
			strings.TrimSpace(stripped[start:end]))
	}
	return nil
}

// Render 把佔位符代入真值。
func Render(text string, fields map[string]string) string {
	return placeholderRe.ReplaceAllStringFunc(text, func(m string) string {
		key := placeholderRe.FindStringSubmatch(m)[1]
		return fields[key]
	})
}

const disclaimer = "⚠️ 以上內容僅供學習與理解市場動態使用，非投資建議。"

// Generate 完整流程：prompt → LLM → 驗證（違規回饋重試一次）→ 代入 → 附術語教室與免責。
func Generate(ctx context.Context, p llm.Provider, snap Snapshot) (string, error) {
	fields := snap.Fields()
	userPrompt := buildUserPrompt(snap)

	text, err := generateValidated(ctx, p, userPrompt, fields)
	if err != nil {
		return "", err
	}
	rendered := Render(text, fields)

	// 術語小教室：deterministic 附加（定義以辭典為準，不經 LLM）
	terms, err := glossary.Load()
	if err != nil {
		return "", err
	}
	term := glossary.PickRelevant(terms, rendered, snap.Date.YearDay())

	var b strings.Builder
	fmt.Fprintf(&b, "📊 %s 盤後摘要\n\n%s\n\n", fields["date"], strings.TrimSpace(rendered))
	fmt.Fprintf(&b, "【術語小教室】\n%s：%s\n\n", term.Term, term.Explanation)
	b.WriteString(disclaimer)
	return b.String(), nil
}

// generateValidated 呼叫 LLM，違規時把錯誤餵回重試一次。
func generateValidated(ctx context.Context, p llm.Provider, userPrompt string, fields map[string]string) (string, error) {
	resp, err := p.Generate(ctx, llm.GenerateRequest{
		SystemPrompt: systemPrompt,
		UserPrompt:   userPrompt,
		MaxTokens:    1024,
	})
	if err != nil {
		return "", err
	}
	if verr := Validate(resp.Text, fields); verr != nil {
		retry, err := p.Generate(ctx, llm.GenerateRequest{
			SystemPrompt: systemPrompt,
			UserPrompt: userPrompt + fmt.Sprintf(
				"\n\n你上一次的輸出違反規則：%v。請修正後重新輸出完整摘要。", verr),
			MaxTokens: 1024,
		})
		if err != nil {
			return "", err
		}
		if verr := Validate(retry.Text, fields); verr != nil {
			return "", fmt.Errorf("report: LLM 重試後仍違規：%w", verr)
		}
		return retry.Text, nil
	}
	return resp.Text, nil
}
