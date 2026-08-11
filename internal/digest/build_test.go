package digest_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brucechen520/daily-stock/internal/digest"
	"github.com/brucechen520/daily-stock/internal/llm"
	"github.com/brucechen520/daily-stock/internal/store"
)

var tradeDay = time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)

type fakeRepo struct {
	watch []store.WatchItem
	bars  map[string][]store.StockDaily
	saved *[]store.Summary
}

func (f fakeRepo) SaveSummary(_ context.Context, sm store.Summary) error {
	if f.saved != nil {
		*f.saved = append(*f.saved, sm)
	}
	return nil
}

func (f fakeRepo) LatestTradeDate(context.Context) (time.Time, error) { return tradeDay, nil }

func (f fakeRepo) GetIndexDaily(context.Context, time.Time) (*store.IndexDaily, error) {
	return &store.IndexDaily{TradeDate: tradeDay, Close: 44928.76, Change: 702.85}, nil
}

func (f fakeRepo) GetInstitutional(context.Context, time.Time) ([]store.Institutional, error) {
	return []store.Institutional{
		{TradeDate: tradeDay, Actor: "foreign", NetAmount: 51_740_515_132},
		{TradeDate: tradeDay, Actor: "trust", NetAmount: 3_401_069_421},
		{TradeDate: tradeDay, Actor: "dealer", NetAmount: 18_468_154_345},
	}, nil
}

func (f fakeRepo) ListWatchlist(context.Context) ([]store.WatchItem, error) { return f.watch, nil }

func (f fakeRepo) RecentStockDaily(_ context.Context, _ []string, _ time.Time, _ int) (map[string][]store.StockDaily, error) {
	return f.bars, nil
}

// fakeLLM 回傳只含佔位符的合法輸出（report.Validate 會擋掉裸數字），
// 並記錄收到的 request——持倉成本不得出現在 prompt 裡（§5.3），那要直接斷言。
type fakeLLM struct{ seen *[]llm.GenerateRequest }

func (f fakeLLM) Generate(_ context.Context, req llm.GenerateRequest) (*llm.GenerateResponse, error) {
	if f.seen != nil {
		*f.seen = append(*f.seen, req)
	}
	return &llm.GenerateResponse{
		Text: "加權指數收 {index_close} 點，{index_change_desc}。外資{foreign_net_desc}。",
	}, nil
}
func (fakeLLM) Name() (string, string) { return "fake", "fake-1" }

func price(v float64) *float64 { return &v }

func bar(symbol, name string, d time.Time, close float64) store.StockDaily {
	return store.StockDaily{Symbol: symbol, Name: name, TradeDate: d, Close: price(close), Volume: 1000}
}

func TestBuild_ComposesMarketSummaryAndHoldings(t *testing.T) {
	repo := fakeRepo{
		watch: []store.WatchItem{{Symbol: "2330", Shares: 2000}},
		bars: map[string][]store.StockDaily{
			"2330": {
				bar("2330", "台積電", tradeDay, 1100),
				bar("2330", "台積電", tradeDay.AddDate(0, 0, -1), 1000),
			},
		},
	}

	got, err := digest.Build(context.Background(), repo, fakeLLM{}, time.Time{}, true)

	if err != nil {
		t.Fatalf("Build 回傳非預期錯誤: %v", err)
	}
	if !got.Date.Equal(tradeDay) {
		t.Errorf("Date = %v, want %v（零值應取 LatestTradeDate）", got.Date, tradeDay)
	}
	body := sectionsText(got.Sections)
	for _, want := range []string{"44,928.76", "台積電", "+10.00%", "未還原", "非投資建議"} {
		if !strings.Contains(body, want) {
			t.Errorf("推播內容缺少 %q：\n%s", want, body)
		}
	}
}

// 摘要必須落地——排程走的是 digest 這條路，漏了落地會整週沒人發現，
// 而 summaries 是 Phase 4 前端與 eval 的素材來源。
func TestBuild_PersistsGeneratedSummary(t *testing.T) {
	var saved []store.Summary
	repo := fakeRepo{saved: &saved}

	if _, err := digest.Build(context.Background(), repo, fakeLLM{}, tradeDay, false); err != nil {
		t.Fatalf("Build 回傳非預期錯誤: %v", err)
	}

	if len(saved) != 1 {
		t.Fatalf("落地 %d 筆摘要, want 1", len(saved))
	}
	if !saved[0].TradeDate.Equal(tradeDay) {
		t.Errorf("TradeDate = %v, want %v", saved[0].TradeDate, tradeDay)
	}
	if saved[0].Provider != "fake" || saved[0].Model != "fake-1" {
		t.Errorf("provider/model = %s/%s, want fake/fake-1", saved[0].Provider, saved[0].Model)
	}
	if !strings.Contains(saved[0].Content, "44,928.76") {
		t.Errorf("落地內容應為代入後的摘要：%q", saved[0].Content)
	}
}

// 持倉成本絕不能流進 LLM prompt 或推播文字（§5.3）。
func TestBuild_NeverLeaksCostIntoOutput(t *testing.T) {
	cost := 985.0
	boughtAt := tradeDay.AddDate(-1, 0, 0)
	repo := fakeRepo{
		watch: []store.WatchItem{{Symbol: "2330", Shares: 2000, AvgCost: &cost, BoughtAt: &boughtAt}},
		bars: map[string][]store.StockDaily{
			"2330": {bar("2330", "台積電", tradeDay, 1100), bar("2330", "台積電", tradeDay.AddDate(0, 0, -1), 1000)},
		},
	}

	var prompts []llm.GenerateRequest
	got, err := digest.Build(context.Background(), repo, fakeLLM{seen: &prompts}, tradeDay, false)

	if err != nil {
		t.Fatalf("Build 回傳非預期錯誤: %v", err)
	}
	body := sectionsText(got.Sections)
	for _, leaked := range []string{"985", "2000"} {
		if strings.Contains(body, leaked) {
			t.Errorf("推播內容洩漏持倉資料 %q：\n%s", leaked, body)
		}
	}
	if len(prompts) == 0 {
		t.Fatal("沒有攔到任何 LLM request")
	}
	for _, req := range prompts {
		payload := req.SystemPrompt + req.UserPrompt
		for _, leaked := range []string{"985", "2000", "2025-03-14"} {
			if strings.Contains(payload, leaked) {
				t.Errorf("LLM prompt 洩漏持倉資料 %q：\n%s", leaked, payload)
			}
		}
	}
}

// watchlist 有、但 stock_daily 沒有（沒回補、或非上市股票）——
// 必須列出來說「查無」，靜默省略會讓人以為那檔今天沒事。
func TestBuild_ListsWatchedSymbolWithoutPriceData(t *testing.T) {
	repo := fakeRepo{
		watch: []store.WatchItem{{Symbol: "6488"}},
		bars:  map[string][]store.StockDaily{},
	}

	got, err := digest.Build(context.Background(), repo, fakeLLM{}, tradeDay, false)

	if err != nil {
		t.Fatalf("Build 回傳非預期錯誤: %v", err)
	}
	body := sectionsText(got.Sections)
	if !strings.Contains(body, "6488") || !strings.Contains(body, "查無") {
		t.Errorf("缺資料的個股應被列出並標示查無：\n%s", body)
	}
}

type erroringRepo struct{ fakeRepo }

func (erroringRepo) LatestTradeDate(context.Context) (time.Time, error) {
	return time.Time{}, errors.New("no rows in result set")
}

func TestBuild_SurfacesMissingDataAsError(t *testing.T) {
	_, err := digest.Build(context.Background(), erroringRepo{}, fakeLLM{}, time.Time{}, false)

	if err == nil {
		t.Fatal("查不到交易日應回錯誤（呼叫端才能推失敗訊息）")
	}
	if !strings.Contains(err.Error(), "ingest") {
		t.Errorf("錯誤訊息應提示先跑 ingest: %v", err)
	}
}
