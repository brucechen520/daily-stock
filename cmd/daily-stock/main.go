// daily-stock — 台股盤後白話摘要工具（Phase 1）。
//
// 子指令：
//
//	daily-stock schedule                          長駐排程（平日盤後自動抓 + 生成、新聞每 30 分）
//	daily-stock ingest                            手動抓當日市場資料
//	daily-stock news                              手動抓一輪新聞
//	daily-stock backfill --symbol 2330 --from 2025-01 [--to 2025-12]
//	daily-stock summary [--date 2026-08-06]       生成（或重生成）某日白話摘要
//	daily-stock eval                              跑 LLM 輸出忠實度 golden set，結果落 eval_runs
//	daily-stock reparse                           重放 raw_payloads（解析 bug 修復後用）
//
// 設定走環境變數 / .env（見 .env.example）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/brucechen520/daily-stock/internal/config"
	"github.com/brucechen520/daily-stock/internal/eval"
	"github.com/brucechen520/daily-stock/internal/infra"
	"github.com/brucechen520/daily-stock/internal/ingest"
	"github.com/brucechen520/daily-stock/internal/llm"
	"github.com/brucechen520/daily-stock/internal/market"
	"github.com/brucechen520/daily-stock/internal/news"
	"github.com/brucechen520/daily-stock/internal/report"
	"github.com/brucechen520/daily-stock/internal/scheduler"
	"github.com/brucechen520/daily-stock/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	cfg := config.Load()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	app, err := newApp(ctx, cfg)
	if err != nil {
		fatalf("初始化失敗：%v", err)
	}
	defer app.close()

	switch cmd {
	case "schedule":
		err = app.runSchedule(ctx)
	case "ingest":
		err = app.svc.IngestMarket(ctx)
	case "news":
		err = app.svc.IngestNews(ctx)
	case "backfill":
		err = app.runBackfill(ctx, args)
	case "summary":
		err = app.runSummary(ctx, args)
	case "eval":
		err = app.runEval(ctx)
	case "reparse":
		err = app.svc.Reparse(ctx)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil && ctx.Err() == nil {
		fatalf("%s 失敗：%v", cmd, err)
	}
}

// app 聚合所有相依，各子指令共用。
type app struct {
	cfg config.Config
	st  *store.Store
	inf *infra.Infra
	svc *ingest.Service
}

func newApp(ctx context.Context, cfg config.Config) (*app, error) {
	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("連 Postgres（docker compose up -d 了嗎？）：%w", err)
	}
	inf, err := infra.New(cfg.RedisAddr, cfg.RedisPassword)
	if err != nil {
		log.Printf("[infra] Redis 連線失敗（%v），lock/限速降為 no-op", err)
		inf, _ = infra.New("", "")
	}
	svc := &ingest.Service{
		TWSE:  market.NewTWSEClient(inf.OutboundLimiter()),
		News:  news.NewFetcher(cfg.NewsFeeds),
		Store: st,
	}
	return &app{cfg: cfg, st: st, inf: inf, svc: svc}, nil
}

func (a *app) close() {
	a.inf.Close()
	a.st.Close()
}

func (a *app) provider() (llm.Provider, error) {
	return llm.New(llm.Config{
		Provider:        a.cfg.LLMProvider,
		AnthropicAPIKey: a.cfg.AnthropicAPIKey,
		AnthropicModel:  a.cfg.AnthropicModel,
		GeminiAPIKey:    a.cfg.GeminiAPIKey,
		GeminiModel:     a.cfg.GeminiModel,
		OllamaURL:       a.cfg.OllamaURL,
		OllamaModel:     a.cfg.OllamaModel,
	})
}

func (a *app) runSchedule(ctx context.Context) error {
	return scheduler.New(a.inf, scheduler.Jobs{
		IngestMarket: a.svc.IngestMarket,
		IngestNews:   a.svc.IngestNews,
		GenerateSummary: func(ctx context.Context) error {
			_, err := a.generateSummary(ctx, time.Time{}) // 零值 = 最新交易日
			return err
		},
	}).Run(ctx)
}

func (a *app) runBackfill(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ExitOnError)
	symbol := fs.String("symbol", "", "股票代號（必填）")
	from := fs.String("from", "", "起始月 YYYY-MM（必填）")
	to := fs.String("to", "", "結束月 YYYY-MM（預設本月）")
	_ = fs.Parse(args)
	if *symbol == "" || *from == "" {
		return fmt.Errorf("--symbol 與 --from 必填")
	}
	fromT, err := time.Parse("2006-01", *from)
	if err != nil {
		return fmt.Errorf("--from 格式要 YYYY-MM：%w", err)
	}
	toT := time.Now()
	if *to != "" {
		if toT, err = time.Parse("2006-01", *to); err != nil {
			return fmt.Errorf("--to 格式要 YYYY-MM：%w", err)
		}
	}
	return a.svc.Backfill(ctx, *symbol, fromT, toT)
}

func (a *app) runSummary(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("summary", flag.ExitOnError)
	dateStr := fs.String("date", "", "交易日 YYYY-MM-DD（預設 DB 內最新）")
	_ = fs.Parse(args)
	var date time.Time
	if *dateStr != "" {
		var err error
		if date, err = time.Parse("2006-01-02", *dateStr); err != nil {
			return fmt.Errorf("--date 格式要 YYYY-MM-DD：%w", err)
		}
	}
	out, err := a.generateSummary(ctx, date)
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

// generateSummary 讀 pg 當日資料 → LLM 生成 → 落 summaries。date 零值 = 最新交易日。
func (a *app) generateSummary(ctx context.Context, date time.Time) (string, error) {
	if date.IsZero() {
		var err error
		if date, err = a.st.LatestTradeDate(ctx); err != nil {
			return "", fmt.Errorf("查最新交易日（先跑 ingest？）：%w", err)
		}
	}
	idx, err := a.st.GetIndexDaily(ctx, date)
	if err != nil {
		return "", fmt.Errorf("查 %s 大盤資料（先跑 ingest？）：%w", date.Format("2006-01-02"), err)
	}
	instRows, err := a.st.GetInstitutional(ctx, date)
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

	p, err := a.provider()
	if err != nil {
		return "", err
	}
	out, err := report.Generate(ctx, p, snap)
	if err != nil {
		return "", err
	}
	provider, model := p.Name()
	if err := a.st.SaveSummary(ctx, store.Summary{
		TradeDate: date, Provider: provider, Model: model, Content: out,
	}); err != nil {
		return "", fmt.Errorf("摘要落地：%w", err)
	}
	return out, nil
}

func (a *app) runEval(ctx context.Context) error {
	p, err := a.provider()
	if err != nil {
		return err
	}
	res, err := eval.Run(ctx, p)
	if err != nil {
		return err
	}
	results := make([]store.EvalCaseResult, len(res.Cases))
	for i, c := range res.Cases {
		results[i] = store.EvalCaseResult{CaseName: c.Name, Passed: c.Passed, Detail: c.Detail}
		status := "✅"
		if !c.Passed {
			status = "❌"
		}
		fmt.Printf("%s %s", status, c.Name)
		if c.Detail != "" {
			fmt.Printf("（%s）", c.Detail)
		}
		fmt.Println()
	}
	runID, err := a.st.SaveEvalRun(ctx, res.Provider, res.Model, results)
	if err != nil {
		return fmt.Errorf("eval 結果落地：%w", err)
	}
	fmt.Printf("\npass rate: %d/%d（run #%d，provider=%s model=%s）\n",
		res.Passed(), len(res.Cases), runID, res.Provider, res.Model)
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `daily-stock <指令>

  schedule    長駐排程（平日 17:30 抓資料、18:30 生成摘要、每 30 分鐘抓新聞）
  ingest      手動抓當日市場資料
  news        手動抓一輪新聞
  backfill    回補個股歷史  --symbol 2330 --from 2025-01 [--to 2025-12]
  summary     生成某日摘要  [--date 2026-08-06]
  eval        跑 LLM 輸出忠實度 golden set
  reparse     重放 raw_payloads（解析修復後）`)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
