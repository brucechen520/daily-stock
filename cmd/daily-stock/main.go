// daily-stock — 台股盤後白話摘要與自選股追蹤工具（Phase 1 / 1.5）。
//
// 子指令：
//
//	daily-stock schedule                          長駐排程（平日盤後自動抓 + 生成推播、新聞每 30 分）
//	daily-stock ingest                            手動抓當日市場資料
//	daily-stock news                              手動抓一輪新聞
//	daily-stock backfill --symbol 2330 --from 2025-01 [--to 2025-12]
//	daily-stock summary [--date 2026-08-06]       生成（或重生成）某日白話摘要
//	daily-stock push [--date 2026-08-06] [--dry]  組出當日推播並送到 Discord
//	daily-stock watch [add|remove] …              管理自選股清單
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
	"github.com/brucechen520/daily-stock/internal/digest"
	"github.com/brucechen520/daily-stock/internal/eval"
	"github.com/brucechen520/daily-stock/internal/infra"
	"github.com/brucechen520/daily-stock/internal/ingest"
	"github.com/brucechen520/daily-stock/internal/llm"
	"github.com/brucechen520/daily-stock/internal/market"
	"github.com/brucechen520/daily-stock/internal/news"
	"github.com/brucechen520/daily-stock/internal/notify"
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
	case "push":
		err = app.runPush(ctx, args)
	case "watch":
		err = app.runWatch(ctx, args)
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
	cfg  config.Config
	st   *store.Store
	inf  *infra.Infra
	svc  *ingest.Service
	push *notify.Discord
}

// warnUnadjusted 在還原股價（Phase 1.5 週 3）完成前為 true——
// 未還原的數字在除權息日會假跌，推播必須誠實標註。週 3 改成 false 並刪掉這個常數。
const warnUnadjusted = true

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
	return &app{
		cfg:  cfg,
		st:   st,
		inf:  inf,
		svc:  svc,
		push: notify.NewDiscord(cfg.DiscordWebhookURL),
	}, nil
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
		DailyDigest: func(ctx context.Context) error {
			return a.pushDigest(ctx, time.Time{}, false) // 零值 = 最新交易日
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

// runSummary 只印大盤摘要（不含自選股）。走的是與 push 相同的 digest.Build，
// 所以「生成 → 落地 summaries」的順序只有一份實作。
func (a *app) runSummary(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("summary", flag.ExitOnError)
	dateStr := fs.String("date", "", "交易日 YYYY-MM-DD（預設 DB 內最新）")
	_ = fs.Parse(args)
	date, err := parseDate(*dateStr)
	if err != nil {
		return err
	}
	p, err := a.provider()
	if err != nil {
		return err
	}
	res, err := digest.Build(ctx, a.st, p, date, warnUnadjusted)
	if err != nil {
		return err
	}
	fmt.Println(res.Summary)
	return nil
}

func (a *app) runPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	dateStr := fs.String("date", "", "交易日 YYYY-MM-DD（預設 DB 內最新）")
	dry := fs.Bool("dry", false, "只印出推播內容，不真的送")
	_ = fs.Parse(args)
	date, err := parseDate(*dateStr)
	if err != nil {
		return err
	}
	return a.pushDigest(ctx, date, *dry)
}

// pushDigest 組出當日推播並送出。
//
// 失敗處理（docs/phase-1.5.md §4.3）：任何一步失敗都改推一則失敗訊息——
// 靜默是最糟的失敗模式，沒收到通知會分不清「今天沒事」與「排程死了」。
func (a *app) pushDigest(ctx context.Context, date time.Time, dry bool) error {
	sections, buildErr := a.buildSections(ctx, date)
	messages := notify.Pack(sections, notify.DiscordLimit)

	if dry {
		for i, m := range messages {
			fmt.Printf("─── 第 %d/%d 則 ───\n%s\n\n", i+1, len(messages), m)
		}
		return buildErr
	}

	if sendErr := a.push.Send(ctx, messages); sendErr != nil {
		// 已送出的不重送（重複訊息洗頻道比缺一則更煩），改補一則說明推播不完整。
		notice := notify.Pack([]notify.Section{{
			Body: fmt.Sprintf("⚠️ 推播不完整：%v", sendErr),
		}}, notify.DiscordLimit)
		if err := a.push.Send(ctx, notice); err != nil {
			log.Printf("[push] 連「推播不完整」通知都送不出去：%v", err)
		}
		return sendErr
	}
	if buildErr != nil {
		return buildErr
	}
	log.Printf("[push] 推播完成（%d 則）", len(messages))
	return nil
}

// buildSections 決定今天要推什麼。三種結果：正常摘要、資料未到的簡短通知、失敗訊息。
func (a *app) buildSections(ctx context.Context, date time.Time) ([]notify.Section, error) {
	p, err := a.provider()
	if err != nil {
		return digest.FailureSections(fallbackDate(date), err.Error()), err
	}
	res, err := digest.Build(ctx, a.st, p, date, warnUnadjusted)
	if err != nil {
		return digest.FailureSections(fallbackDate(date), err.Error()), err
	}
	// date 是零值代表「用 DB 內最新交易日」。若那天不是今天，資料就是舊的——
	// 直接推會把昨天的數字掛上今天的日期，正是 PRD FR-1.6 要防的「過期資料冒充最新」。
	if date.IsZero() && !sameTaipeiDay(res.Date, time.Now()) {
		return digest.StaleSections(res.Date), nil
	}
	return res.Sections, nil
}

// sameTaipeiDay 以台北時區比對日曆日（交易日的定義在台北，不在 UTC）。
func sameTaipeiDay(a, b time.Time) bool {
	loc, err := time.LoadLocation("Asia/Taipei")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	ay, am, ad := a.In(loc).Date()
	by, bm, bd := b.In(loc).Date()
	return ay == by && am == bm && ad == bd
}

func (a *app) runWatch(ctx context.Context, args []string) error {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "", "list":
		return a.watchList(ctx)
	case "add":
		return a.watchAdd(ctx, args)
	case "remove", "rm":
		return a.watchRemove(ctx, args)
	default:
		return fmt.Errorf("未知的 watch 子指令 %q（list | add | remove）", sub)
	}
}

func (a *app) watchList(ctx context.Context) error {
	items, err := a.st.ListWatchlist(ctx)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Println("自選股清單是空的。用 `daily-stock watch add --symbol 2330` 或 data/positions.local.sql 建立。")
		return nil
	}
	fmt.Printf("%-8s %10s %10s %12s  %s\n", "代號", "股數", "均價", "買進日", "備註")
	for _, w := range items {
		cost, bought := "—", "—"
		if w.AvgCost != nil {
			cost = fmt.Sprintf("%.2f", *w.AvgCost)
		}
		if w.BoughtAt != nil {
			bought = w.BoughtAt.Format("2006-01-02")
		}
		fmt.Printf("%-8s %10d %10s %12s  %s\n", w.Symbol, w.Shares, cost, bought, w.Note)
	}
	fmt.Printf("\n共 %d 檔\n", len(items))
	return nil
}

func (a *app) watchAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("watch add", flag.ExitOnError)
	symbol := fs.String("symbol", "", "股票代號（必填）")
	shares := fs.Int64("shares", 0, "持有股數（1 張 = 1000 股）；0 = 只觀察")
	cost := fs.Float64("cost", 0, "平均成本；0 = 不記錄")
	bought := fs.String("bought", "", "首次買進日 YYYY-MM-DD")
	note := fs.String("note", "", "備註")
	_ = fs.Parse(args)
	if *symbol == "" {
		return fmt.Errorf("--symbol 必填")
	}
	item := store.WatchItem{Symbol: *symbol, Shares: *shares, Note: *note}
	if *cost > 0 {
		item.AvgCost = cost
	}
	if *bought != "" {
		d, err := time.Parse("2006-01-02", *bought)
		if err != nil {
			return fmt.Errorf("--bought 格式要 YYYY-MM-DD：%w", err)
		}
		item.BoughtAt = &d
	}
	if err := a.st.UpsertWatch(ctx, item); err != nil {
		return err
	}
	fmt.Printf("已加入/更新 %s\n", item.Symbol)
	return nil
}

func (a *app) watchRemove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("watch remove", flag.ExitOnError)
	symbol := fs.String("symbol", "", "股票代號（必填）")
	_ = fs.Parse(args)
	if *symbol == "" {
		return fmt.Errorf("--symbol 必填")
	}
	removed, err := a.st.RemoveWatch(ctx, *symbol)
	if err != nil {
		return err
	}
	if !removed {
		fmt.Printf("%s 本來就不在清單裡\n", *symbol)
		return nil
	}
	fmt.Printf("已移除 %s\n", *symbol)
	return nil
}

func parseDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--date 格式要 YYYY-MM-DD：%w", err)
	}
	return d, nil
}

// fallbackDate 讓失敗訊息至少帶得出一個日期（連交易日都查不到時用今天）。
func fallbackDate(date time.Time) time.Time {
	if date.IsZero() {
		return time.Now()
	}
	return date
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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

  schedule    長駐排程（平日 17:30 抓資料、18:30 生成推播、每 30 分鐘抓新聞）
  ingest      手動抓當日市場資料
  news        手動抓一輪新聞
  backfill    回補個股歷史  --symbol 2330 --from 2025-01 [--to 2025-12]
  summary     生成某日摘要  [--date 2026-08-06]
  push        組出當日推播並送到 Discord  [--date 2026-08-06] [--dry]
  watch       自選股清單    list | add --symbol 2330 [--shares 1000 --cost 985 --bought 2025-03-14] | remove --symbol 2330
  eval        跑 LLM 輸出忠實度 golden set
  reparse     重放 raw_payloads（解析修復後）`)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
