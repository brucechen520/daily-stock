package digest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/brucechen520/daily-stock/internal/digest"
	"github.com/brucechen520/daily-stock/internal/indicator"
	"github.com/brucechen520/daily-stock/internal/notify"
)

func holding(symbol, name string, close, pct float64) digest.Holding {
	return digest.Holding{
		Symbol: symbol, Name: name, HasStat: true,
		Stat: indicator.DailyStat{Close: close, ChangePct: pct, HasChange: true},
	}
}

func TestHoldingLine_ShowsSymbolNameCloseAndSignedPct(t *testing.T) {
	got := digest.HoldingLine(holding("2330", "台積電", 1100, 2.14))

	for _, want := range []string{"2330", "台積電", "1,100.00", "+2.14%"} {
		if !strings.Contains(got, want) {
			t.Errorf("持股行缺少 %q：%q", want, got)
		}
	}
}

func TestHoldingLine_NegativeChangeKeepsSign(t *testing.T) {
	got := digest.HoldingLine(holding("2317", "鴻海", 198.5, -1.5))

	if !strings.Contains(got, "-1.50%") {
		t.Errorf("跌幅應保留負號：%q", got)
	}
}

// 剛加入 watchlist、還沒回補歷史時算不出漲跌幅——要顯示「—」而不是 0.00%，
// 0% 會被誤讀成「今天平盤」。
func TestHoldingLine_MarksMissingChangeInsteadOfZero(t *testing.T) {
	h := digest.Holding{
		Symbol: "6505", Name: "台塑化", HasStat: true,
		Stat: indicator.DailyStat{Close: 80, HasChange: false},
	}

	got := digest.HoldingLine(h)

	if strings.Contains(got, "0.00%") {
		t.Errorf("無前一日資料時不可顯示 0.00%%：%q", got)
	}
	if !strings.Contains(got, "—") {
		t.Errorf("無資料應以「—」標示：%q", got)
	}
}

func TestHoldingLine_NoDataAtAll(t *testing.T) {
	got := digest.HoldingLine(digest.Holding{Symbol: "1234", Name: "", HasStat: false})

	if !strings.Contains(got, "1234") {
		t.Errorf("即使無資料也要列出代號（否則會以為漏抓）：%q", got)
	}
	if !strings.Contains(got, "查無") {
		t.Errorf("應明講查無資料：%q", got)
	}
}

func TestSections_OrdersHoldingsByChangeDescending(t *testing.T) {
	secs := digest.Sections("大盤摘要", []digest.Holding{
		holding("A", "甲", 10, -3),
		holding("B", "乙", 10, 5),
		holding("C", "丙", 10, 1),
	}, true)

	body := sectionsText(secs)
	posB, posC, posA := strings.Index(body, "B "), strings.Index(body, "C "), strings.Index(body, "A ")
	if !(posB < posC && posC < posA) {
		t.Errorf("持股未依漲跌幅由大到小排序：\n%s", body)
	}
}

// 查無資料的排最後——它們不是「跌最多」，混在跌幅段裡會誤導。
func TestSections_PutsUnknownHoldingsLast(t *testing.T) {
	secs := digest.Sections("大盤摘要", []digest.Holding{
		{Symbol: "NODATA", HasStat: false},
		holding("B", "乙", 10, -9),
	}, false)

	body := sectionsText(secs)
	if strings.Index(body, "NODATA") < strings.Index(body, "B ") {
		t.Errorf("查無資料的個股應排在最後：\n%s", body)
	}
}

func TestSections_IncludesMarketSummaryAndDisclaimer(t *testing.T) {
	secs := digest.Sections("加權指數收 44,928.76 點", []digest.Holding{holding("2330", "台積電", 1100, 2)}, false)

	body := sectionsText(secs)
	for _, want := range []string{"44,928.76", "非投資建議"} {
		if !strings.Contains(body, want) {
			t.Errorf("推播內容缺少 %q：\n%s", want, body)
		}
	}
}

// report.Generate 的輸出自帶免責；digest 不該再附一次，否則同一則訊息出現兩行。
func TestSections_DoesNotRepeatDisclaimerAlreadyInSummary(t *testing.T) {
	summary := "加權指數收 44,928.76 點\n\n⚠️ 以上內容僅供學習與理解市場動態使用，非投資建議。"

	body := sectionsText(digest.Sections(summary, nil, false))

	if got := strings.Count(body, "非投資建議"); got != 1 {
		t.Errorf("免責出現 %d 次, want 1：\n%s", got, body)
	}
}

// 週 1～2 的數字是未還原股價，除權息日會假跌——必須明講，週 3 還原完成後移除。
func TestSections_ShowsUnadjustedWarningWhenFlagged(t *testing.T) {
	with := sectionsText(digest.Sections("摘要", nil, true))
	without := sectionsText(digest.Sections("摘要", nil, false))

	if !strings.Contains(with, "未還原") {
		t.Errorf("warnUnadjusted=true 時應有警語：\n%s", with)
	}
	if strings.Contains(without, "未還原") {
		t.Errorf("warnUnadjusted=false 時不該有警語：\n%s", without)
	}
}

// §4.3：持股全無明顯異動時推極簡版，省下逐檔閱讀。
func TestSections_CollapsesHoldingsWhenNothingMoved(t *testing.T) {
	body := sectionsText(digest.Sections("大盤摘要", []digest.Holding{
		holding("2330", "台積電", 1100, 0.4),
		holding("2317", "鴻海", 200, -1.1),
	}, false))

	if strings.Contains(body, "台積電") {
		t.Errorf("無明顯異動時不該逐檔列出：\n%s", body)
	}
	if !strings.Contains(body, "無明顯異動") {
		t.Errorf("應說明持股無明顯異動：\n%s", body)
	}
}

// 只要有一檔越過門檻就回到完整列表——不能只列那一檔，
// 否則看不到「其他檔今天真的沒事」，會懷疑是不是漏抓。
func TestSections_ListsAllHoldingsWhenAnyMoved(t *testing.T) {
	body := sectionsText(digest.Sections("大盤摘要", []digest.Holding{
		holding("2330", "台積電", 1100, 3.5),
		holding("2317", "鴻海", 200, -0.2),
	}, false))

	for _, want := range []string{"台積電", "鴻海"} {
		if !strings.Contains(body, want) {
			t.Errorf("有異動時應列出全部持股，缺 %q：\n%s", want, body)
		}
	}
}

// 查無資料本身就是需要知道的異動——不可被極簡版蓋掉。
func TestSections_DoesNotCollapseWhenDataMissing(t *testing.T) {
	body := sectionsText(digest.Sections("大盤摘要", []digest.Holding{
		holding("2330", "台積電", 1100, 0.1),
		{Symbol: "6488", HasStat: false},
	}, false))

	if !strings.Contains(body, "6488") {
		t.Errorf("查無資料的個股不可被極簡版蓋掉：\n%s", body)
	}
}

func TestSections_NoHoldingsStillReportsMarket(t *testing.T) {
	secs := digest.Sections("大盤摘要", nil, false)

	body := sectionsText(secs)
	if !strings.Contains(body, "大盤摘要") {
		t.Error("watchlist 空的時候仍要推大盤")
	}
	if !strings.Contains(body, "自選股") {
		t.Errorf("應說明 watchlist 是空的，而不是靜默省略：\n%s", body)
	}
}

func sectionsText(secs []notify.Section) string {
	var b strings.Builder
	for _, s := range secs {
		b.WriteString(s.Body)
		b.WriteString("\n")
	}
	return b.String()
}

// 今天的盤後資料還沒到（非交易日、或 TWSE OpenAPI 更新落後）時，
// 絕不能把舊資料掛上今天的日期推出去——那是 PRD FR-1.6 的紅線。
func TestStaleSections_SaysDataNotReadyAndNamesLatestDate(t *testing.T) {
	body := sectionsText(digest.StaleSections(time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)))

	if !strings.Contains(body, "2026-08-10") {
		t.Errorf("應標明手上最新資料是哪一天：%s", body)
	}
	if !strings.Contains(body, "尚未") && !strings.Contains(body, "未到") {
		t.Errorf("應說明今日資料尚未到位：%s", body)
	}
}

// 失敗也要推——靜默的話分不清「今天沒事」與「排程死了」。
func TestFailureSections_MentionsDateAndReason(t *testing.T) {
	secs := digest.FailureSections(time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), "TWSE 回 503")

	body := sectionsText(secs)
	for _, want := range []string{"2026-08-10", "TWSE 回 503"} {
		if !strings.Contains(body, want) {
			t.Errorf("失敗訊息缺少 %q：%s", want, body)
		}
	}
}
