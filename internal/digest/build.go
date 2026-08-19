package digest

import (
	"context"
	"fmt"
	"time"

	"github.com/brucechen520/daily-stock/internal/indicator"
	"github.com/brucechen520/daily-stock/internal/llm"
	"github.com/brucechen520/daily-stock/internal/notify"
	"github.com/brucechen520/daily-stock/internal/report"
	"github.com/brucechen520/daily-stock/internal/store"
)

// Repo 是 digest 需要的資料存取切面（介面定義在使用端，方便注入假資料測試）。
type Repo interface {
	LatestTradeDate(ctx context.Context) (time.Time, error)
	GetIndexDaily(ctx context.Context, date time.Time) (*store.IndexDaily, error)
	GetInstitutional(ctx context.Context, date time.Time) ([]store.Institutional, error)
	ListWatchlist(ctx context.Context) ([]store.WatchItem, error)
	RecentStockDaily(ctx context.Context, symbols []string, asOf time.Time, limit int) (map[string][]store.StockDaily, error)
	SaveSummary(ctx context.Context, sm store.Summary) error
}

// barsNeeded 是每檔要撈幾根K。週 1 只算當日漲跌幅，兩根就夠
// （多撈一根備援：最新那天該檔可能無成交 → close 為 NULL）。
const barsNeeded = 3

// Result 是一次組裝的產出。Date 用於失敗訊息與 log。
type Result struct {
	Date     time.Time
	Summary  string
	Holdings []Holding
	Sections []notify.Section
}

// Build 讀 pg → 生成大盤摘要（LLM）→ 算持股當日統計 → 組 Section。
// date 零值代表用 DB 內最新交易日。
func Build(ctx context.Context, repo Repo, p llm.Provider, date time.Time, warnUnadjusted bool) (*Result, error) {
	if date.IsZero() {
		var err error
		if date, err = repo.LatestTradeDate(ctx); err != nil {
			return nil, fmt.Errorf("查最新交易日（先跑 ingest？）：%w", err)
		}
	}

	summary, err := buildMarketSummary(ctx, repo, p, date)
	if err != nil {
		return nil, err
	}
	holdings, err := buildHoldings(ctx, repo, date)
	if err != nil {
		return nil, err
	}
	return &Result{
		Date:     date,
		Summary:  summary,
		Holdings: holdings,
		Sections: Sections(summary, holdings, warnUnadjusted),
	}, nil
}

// buildMarketSummary 生成大盤白話摘要並落地 summaries——
// 落地是這裡的責任，不是呼叫端的：漏了的話排程跑一整週都不會有人發現，
// 而 summaries 同時是 Phase 4 前端的資料來源與 eval 的素材庫。
func buildMarketSummary(ctx context.Context, repo Repo, p llm.Provider, date time.Time) (string, error) {
	idx, err := repo.GetIndexDaily(ctx, date)
	if err != nil {
		return "", fmt.Errorf("查 %s 大盤資料（先跑 ingest？）：%w", date.Format("2006-01-02"), err)
	}
	instRows, err := repo.GetInstitutional(ctx, date)
	if err != nil {
		return "", err
	}
	snap := report.Snapshot{Date: date, IndexClose: idx.Close, IndexChange: idx.Change}
	for _, r := range instRows {
		switch r.Actor {
		case "foreign":
			snap.ForeignNet = r.NetAmount
		case "trust":
			snap.TrustNet = r.NetAmount
		case "dealer":
			snap.DealerNet = r.NetAmount
		}
	}
	text, err := report.Generate(ctx, p, snap)
	if err != nil {
		return "", err
	}
	provider, model := p.Name()
	if err := repo.SaveSummary(ctx, store.Summary{
		TradeDate: date, Provider: provider, Model: model, Content: text,
	}); err != nil {
		return "", fmt.Errorf("摘要落地：%w", err)
	}
	return text, nil
}

// buildHoldings 只讀行情，**不碰 watchlist 的 avg_cost / bought_at**——
// 那些是敏感欄位，週 3 算報酬率時才會用到，且永遠不進 LLM prompt（§5.3）。
func buildHoldings(ctx context.Context, repo Repo, date time.Time) ([]Holding, error) {
	items, err := repo.ListWatchlist(ctx)
	if err != nil {
		return nil, fmt.Errorf("查 watchlist：%w", err)
	}
	if len(items) == 0 {
		return nil, nil
	}
	symbols := make([]string, len(items))
	for i, it := range items {
		symbols[i] = it.Symbol
	}
	barsBySymbol, err := repo.RecentStockDaily(ctx, symbols, date, barsNeeded)
	if err != nil {
		return nil, fmt.Errorf("查個股日K：%w", err)
	}

	holdings := make([]Holding, 0, len(items))
	for _, it := range items {
		h := Holding{Symbol: it.Symbol}
		rows := barsBySymbol[it.Symbol]
		if len(rows) > 0 {
			h.Name = rows[0].Name
			bars := make([]indicator.Bar, len(rows))
			for i, r := range rows {
				bars[i] = indicator.Bar{TradeDate: r.TradeDate, Close: r.Close, Volume: r.Volume}
			}
			if stat, err := indicator.Daily(bars); err == nil {
				h.Stat, h.HasStat = stat, true
			}
		}
		holdings = append(holdings, h)
	}
	return holdings, nil
}
