// Package digest 是輸出端的組裝層：讀 pg → 算統計 → 組成推播 Section。
// 與 ingest（抓取端的組裝層）對稱——兩者都只做編排，不放領域邏輯。
//
// 分工：
//
//	indicator  算數字
//	report     大盤白話摘要（唯一會碰 LLM 的地方）
//	digest     把上面兩者組成 []notify.Section（render）
//	notify     把 Section 送出去（sender）
//
// 紅線（docs/phase-1.5.md §5）：持股逐檔敘述**純模板生成**，不經 LLM；
// 成本、損益、新聞都不得進入 LLM prompt。
package digest

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/brucechen520/daily-stock/internal/indicator"
	"github.com/brucechen520/daily-stock/internal/notify"
	"github.com/brucechen520/daily-stock/internal/report"
)

// Holding 是 watchlist 中一檔的當日狀態。
// HasStat=false 代表 stock_daily 查不到這檔（沒回補、或非上市股票）。
type Holding struct {
	Symbol  string
	Name    string
	Stat    indicator.DailyStat
	HasStat bool
}

const (
	disclaimer      = "⚠️ 僅供學習與理解市場動態使用，非投資建議。"
	disclaimerMark  = "非投資建議" // 判斷大盤段是否已自帶免責
	unadjustedWarn  = "⚠️ 本階段為未還原股價，除權息日的漲跌幅會失真（Phase 1.5 週 3 修正）。"
	noChangeMark    = "—"
	emptyWatchlist  = "自選股清單是空的（用 `daily-stock watch add` 或 data/positions.local.sql 建立）。"
	holdingsHeading = "【自選股】"
)

// HoldingLine 產生單一持股的一行敘述。純模板，零 LLM。
func HoldingLine(h Holding) string {
	name := strings.TrimSpace(h.Name)
	head := h.Symbol
	if name != "" {
		head += " " + name
	}
	if !h.HasStat {
		return fmt.Sprintf("%s  查無當日資料", head)
	}
	if !h.Stat.HasChange {
		return fmt.Sprintf("%s  %s  %s", head, report.FormatFloat(h.Stat.Close), noChangeMark)
	}
	return fmt.Sprintf("%s  %s  %+.2f%%", head, report.FormatFloat(h.Stat.Close), h.Stat.ChangePct)
}

// Sections 組出整份推播內容。marketSummary 由 report.Generate 產生（已含大盤術語與免責）。
// warnUnadjusted 在還原股價完成前為 true。
func Sections(marketSummary string, holdings []Holding, warnUnadjusted bool) []notify.Section {
	secs := []notify.Section{{Body: strings.TrimSpace(marketSummary)}}

	var body strings.Builder
	body.WriteString(holdingsHeading)
	body.WriteString("\n")
	switch {
	case len(holdings) == 0:
		body.WriteString(emptyWatchlist)
	case quiet(holdings):
		fmt.Fprintf(&body, "%d 檔持股今日無明顯異動（漲跌幅均小於 %.0f%%）。", len(holdings), moveThresholdPct)
	default:
		for _, h := range sortForDisplay(holdings) {
			body.WriteString(HoldingLine(h))
			body.WriteString("\n")
		}
	}
	secs = append(secs, notify.Section{Body: body.String()})

	var tail strings.Builder
	if warnUnadjusted {
		tail.WriteString(unadjustedWarn)
		tail.WriteString("\n")
	}
	// report.Generate 的輸出自帶免責；同一則訊息印兩行免責只是噪音。
	if !strings.Contains(marketSummary, disclaimerMark) {
		tail.WriteString(disclaimer)
	}
	if strings.TrimSpace(tail.String()) == "" {
		return secs
	}
	return append(secs, notify.Section{Body: tail.String()})
}

// StaleSections 用在「今天的盤後資料還沒到」——非交易日，或 TWSE OpenAPI
// 更新落後（實測 16:00 仍停在前一交易日）。
//
// 為什麼不靜默：靜默的話分不清「今天沒開盤」與「排程死了三天」。
// 為什麼不推完整摘要：那會把昨天的數字掛上今天的日期，就是 PRD FR-1.6
// 要防的「過期資料冒充最新」。
//
// 已知取捨：spec §4.3 寫「非交易日不推」，但單靠 DB 分不出「非交易日」與
// 「ingest 失敗」，而後者靜默的代價高得多。接上交易日曆後再收斂成不推。
func StaleSections(latest time.Time) []notify.Section {
	return []notify.Section{{
		Body: fmt.Sprintf("📭 今日盤後資料尚未到位（非交易日，或 TWSE 尚未更新）。手上最新資料為 %s。",
			latest.Format("2006-01-02")),
	}}
}

// FailureSections 是抓取或生成失敗時要推的內容。
// 靜默是最糟的失敗模式——沒收到通知時分不清「今天沒事」與「排程死了」。
func FailureSections(date time.Time, reason string) []notify.Section {
	return []notify.Section{{
		Body: fmt.Sprintf("❌ %s 盤後摘要產生失敗：%s", date.Format("2006-01-02"), reason),
	}}
}

// moveThresholdPct 是「有異動」的門檻。週 2 補上量能比與法人連買後，
// 那些也會成為異動條件之一（單看漲跌幅會漏掉量爆但價平的情況）。
const moveThresholdPct = 3.0

// quiet 判斷是否整批持股都沒有值得逐檔細看的變化。
// 查無資料視為「有異動」——那本身就是需要知道的事，不可被極簡版蓋掉。
func quiet(holdings []Holding) bool {
	for _, h := range holdings {
		if !h.HasStat || !h.Stat.HasChange {
			return false
		}
		if h.Stat.ChangePct >= moveThresholdPct || h.Stat.ChangePct <= -moveThresholdPct {
			return false
		}
	}
	return true
}

// sortForDisplay 依漲跌幅由大到小；查無資料的排最後
// （它們不是「跌最多」，混在跌幅段裡會誤導）。
func sortForDisplay(holdings []Holding) []Holding {
	out := append([]Holding(nil), holdings...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.HasStat != b.HasStat {
			return a.HasStat
		}
		if a.Stat.HasChange != b.Stat.HasChange {
			return a.Stat.HasChange
		}
		return a.Stat.ChangePct > b.Stat.ChangePct
	})
	return out
}
