// store 的整合測試：testcontainers 起真 pg（pgvector image，與 docker-compose 同版），
// 驗兩件 Phase 1 對外承諾的事——migration 可重放、寫入冪等。
//
// 需要 docker。跑 `go test -short ./...` 或設 SKIP_DOCKER_TESTS=1 可跳過；
// 兩者都沒設而 docker 起不來時**直接失敗**，不靜默跳過（否則「測試綠燈」會是假的）。
package store_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/brucechen520/daily-stock/internal/store"
)

// 整包測試共用一個容器：起 pg 約 2–5 秒，每個 test 各起一次不划算。
// 隔離改由每個 test 開頭 TRUNCATE 全表達成。
var dsn string

func TestMain(m *testing.M) {
	// -short 要在 m.Run 之前讀，得自己先 parse flag。
	testing.Init()
	flag.Parse()
	if testing.Short() || os.Getenv("SKIP_DOCKER_TESTS") != "" {
		fmt.Println("store: 跳過整合測試（-short 或 SKIP_DOCKER_TESTS）")
		os.Exit(0)
	}

	ctx := context.Background()
	container, err := tcpostgres.Run(ctx,
		"pgvector/pgvector:pg17", // 與 docker-compose.yaml 同一個 image：001 有 CREATE EXTENSION vector
		tcpostgres.WithDatabase("stock"),
		tcpostgres.WithUsername("stock"),
		tcpostgres.WithPassword("testpass"),
		// 不用 BasicWaitStrategies()：它的預設 timeout 在 `go test ./...` 併發跑八個
		// package 時會不夠（pg 已就緒但 docker inspect 排隊排到逾時）。
		// occurrence=2 是因為 pg 初始化會先起一次再重啟。
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(3*time.Minute),
		),
		testcontainers.WithLogger(log.New(io.Discard, "", 0)), // 容器 log 只會淹掉測試輸出
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "store: 起 pg 容器失敗（需要 docker；不想跑請用 -short 或 SKIP_DOCKER_TESTS=1）: %v\n", err)
		os.Exit(1)
	}
	dsn, err = container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "store: 取連線字串失敗: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	if err := testcontainers.TerminateContainer(container); err != nil {
		fmt.Fprintf(os.Stderr, "store: 關閉容器失敗: %v\n", err)
	}
	os.Exit(code)
}

// ---------- helpers ----------

// 全表清單。新增 migration 時要一起補，否則測試之間會互相污染。
var allTables = []string{
	"eval_case_results", "eval_runs", "summaries", "news",
	"corporate_actions", "institutional_stock_daily", "watchlist",
	"stock_daily", "institutional_daily", "market_index_daily", "raw_payloads",
}

// newStore 跑 migration、清空全表，回傳乾淨的 Store。
func newStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	ctx := context.Background()

	s, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(s.Close)

	truncateAll(t, ctx)
	return s, ctx
}

// truncateAll 走獨立連線（Store 不對外露出 pool，測試也不該為此開洞）。
func truncateAll(t *testing.T, ctx context.Context) {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("truncate 用連線池: %v", err)
	}
	defer pool.Close()
	for _, tbl := range allTables {
		if _, err := pool.Exec(ctx, "TRUNCATE TABLE "+tbl+" RESTART IDENTITY CASCADE"); err != nil {
			t.Fatalf("TRUNCATE %s: %v", tbl, err)
		}
	}
}

// countRows 用獨立連線數某表列數。
func countRows(t *testing.T, ctx context.Context, table string) int {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("count 用連線池: %v", err)
	}
	defer pool.Close()
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func day(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

func f(v float64) *float64 { return &v }

// ---------- migration ----------

// store.New 是每個 CLI 指令的第一步，一天會跑很多次；goose 必須跳過已套用版本而不是重跑。
func TestNew_MigrationsAreRerunnableAndPreserveData(t *testing.T) {
	s, ctx := newStore(t)

	if err := s.UpsertIndexDaily(ctx, []store.IndexDaily{
		{TradeDate: day(2026, 8, 10), Close: 24000.5, Change: 120.25, Volume: 7_000_000_000, Amount: 450_000_000_000},
	}); err != nil {
		t.Fatalf("UpsertIndexDaily: %v", err)
	}

	// 第二次 New = 再跑一次 migrate。
	s2, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("第二次 store.New（migration 不可重放）: %v", err)
	}
	defer s2.Close()

	got, err := s2.GetIndexDaily(ctx, day(2026, 8, 10))
	if err != nil {
		t.Fatalf("重跑 migration 後資料不見了: %v", err)
	}
	if got.Close != 24000.5 {
		t.Errorf("Close = %v, want 24000.5（重跑 migration 不該動到資料）", got.Close)
	}
}

// ---------- 冪等：normalized 層 ----------

// 排程 17:30 抓失敗會重試 3 次，同日重跑必須不長列數、且以最新值覆寫。
func TestUpsertStockDaily_IsIdempotentAndOverwrites(t *testing.T) {
	s, ctx := newStore(t)
	d := day(2026, 8, 10)

	first := []store.StockDaily{
		{Symbol: "2330", Name: "台積電", TradeDate: d, Open: f(1000), High: f(1010), Low: f(995), Close: f(1005), Volume: 30_000_000},
		{Symbol: "2317", Name: "鴻海", TradeDate: d, Open: f(200), High: f(203), Low: f(199), Close: f(202), Volume: 50_000_000},
	}
	if err := s.UpsertStockDaily(ctx, first); err != nil {
		t.Fatalf("第一次 UpsertStockDaily: %v", err)
	}

	// TWSE 盤後會修正數字，重抓要蓋掉舊值。
	second := []store.StockDaily{
		{Symbol: "2330", Name: "台積電", TradeDate: d, Open: f(1000), High: f(1010), Low: f(995), Close: f(1008), Volume: 31_000_000},
		{Symbol: "2317", Name: "鴻海", TradeDate: d, Open: f(200), High: f(203), Low: f(199), Close: f(202), Volume: 50_000_000},
	}
	if err := s.UpsertStockDaily(ctx, second); err != nil {
		t.Fatalf("第二次 UpsertStockDaily: %v", err)
	}

	if n := countRows(t, ctx, "stock_daily"); n != 2 {
		t.Errorf("stock_daily 列數 = %d, want 2（重跑不該長列）", n)
	}

	got, err := s.RecentStockDaily(ctx, []string{"2330"}, d, 1)
	if err != nil {
		t.Fatalf("RecentStockDaily: %v", err)
	}
	bars := got["2330"]
	if len(bars) != 1 {
		t.Fatalf("2330 的K數 = %d, want 1", len(bars))
	}
	if bars[0].Close == nil || *bars[0].Close != 1008 {
		t.Errorf("Close = %v, want 1008（第二次寫入的值）", bars[0].Close)
	}
	if bars[0].Volume != 31_000_000 {
		t.Errorf("Volume = %d, want 31000000", bars[0].Volume)
	}
}

// 無成交的股票 TWSE 回 "--"，parse 給 NULL；indicator 靠這個判斷要不要跳過該K。
func TestUpsertStockDaily_KeepsNullCloseForNoTradeDays(t *testing.T) {
	s, ctx := newStore(t)
	d := day(2026, 8, 10)

	if err := s.UpsertStockDaily(ctx, []store.StockDaily{
		{Symbol: "9999", Name: "冷門股", TradeDate: d, Volume: 0},
	}); err != nil {
		t.Fatalf("UpsertStockDaily: %v", err)
	}

	got, err := s.RecentStockDaily(ctx, []string{"9999"}, d, 1)
	if err != nil {
		t.Fatalf("RecentStockDaily: %v", err)
	}
	if len(got["9999"]) != 1 {
		t.Fatalf("9999 的K數 = %d, want 1", len(got["9999"]))
	}
	if got["9999"][0].Close != nil {
		t.Errorf("Close = %v, want nil（無成交日必須留 NULL，不能變成 0）", *got["9999"][0].Close)
	}
}

// RecentStockDaily 一次查整批 watchlist，每檔各自取最近 N 根、由新到舊。
func TestRecentStockDaily_LimitsPerSymbolAndSortsDesc(t *testing.T) {
	s, ctx := newStore(t)

	var rows []store.StockDaily
	for _, sym := range []string{"2330", "2317"} {
		for i := 1; i <= 5; i++ {
			rows = append(rows, store.StockDaily{
				Symbol: sym, Name: sym, TradeDate: day(2026, 8, i),
				Close: f(float64(100 + i)), Volume: int64(i) * 1000,
			})
		}
	}
	if err := s.UpsertStockDaily(ctx, rows); err != nil {
		t.Fatalf("UpsertStockDaily: %v", err)
	}

	// asOf 卡在 8/4：8/5 那根不該被撈到。
	got, err := s.RecentStockDaily(ctx, []string{"2330", "2317"}, day(2026, 8, 4), 2)
	if err != nil {
		t.Fatalf("RecentStockDaily: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("回傳檔數 = %d, want 2", len(got))
	}
	for sym, bars := range got {
		if len(bars) != 2 {
			t.Errorf("%s 的K數 = %d, want 2（limit 是每檔各自算）", sym, len(bars))
			continue
		}
		if !bars[0].TradeDate.Equal(day(2026, 8, 4)) {
			t.Errorf("%s 第一根 = %v, want 8/4（由新到舊且不含 asOf 之後）", sym, bars[0].TradeDate)
		}
		if !bars[1].TradeDate.Equal(day(2026, 8, 3)) {
			t.Errorf("%s 第二根 = %v, want 8/3", sym, bars[1].TradeDate)
		}
	}
}

func TestUpsertInstitutional_IsIdempotentPerActor(t *testing.T) {
	s, ctx := newStore(t)
	d := day(2026, 8, 10)

	rows := []store.Institutional{
		{TradeDate: d, Actor: "foreign", BuyAmount: 330_500_000_000, SellAmount: 278_800_000_000, NetAmount: 51_700_000_000},
		{TradeDate: d, Actor: "trust", BuyAmount: 20_000_000_000, SellAmount: 18_000_000_000, NetAmount: 2_000_000_000},
		{TradeDate: d, Actor: "dealer", BuyAmount: 10_000_000_000, SellAmount: 11_000_000_000, NetAmount: -1_000_000_000},
	}
	if err := s.UpsertInstitutional(ctx, rows); err != nil {
		t.Fatalf("第一次 UpsertInstitutional: %v", err)
	}
	rows[0].NetAmount = 60_000_000_000 // 修正後重抓
	if err := s.UpsertInstitutional(ctx, rows); err != nil {
		t.Fatalf("第二次 UpsertInstitutional: %v", err)
	}

	got, err := s.GetInstitutional(ctx, d)
	if err != nil {
		t.Fatalf("GetInstitutional: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("法人列數 = %d, want 3（三個 actor 各一列，重跑不長列）", len(got))
	}
	for _, r := range got {
		if r.Actor == "foreign" && r.NetAmount != 60_000_000_000 {
			t.Errorf("外資 NetAmount = %d, want 60000000000（第二次的值）", r.NetAmount)
		}
	}
}

// ---------- 冪等：raw 層（reparse 的前提）----------

func TestUpsertRawPayload_OverwritesSameKeyAndListsInDateOrder(t *testing.T) {
	s, ctx := newStore(t)

	if err := s.UpsertRawPayload(ctx, "twse", "FMTQIK", day(2026, 8, 10), []byte(`{"v":1}`)); err != nil {
		t.Fatalf("第一次 UpsertRawPayload: %v", err)
	}
	if err := s.UpsertRawPayload(ctx, "twse", "FMTQIK", day(2026, 8, 10), []byte(`{"v":2}`)); err != nil {
		t.Fatalf("第二次 UpsertRawPayload: %v", err)
	}
	if err := s.UpsertRawPayload(ctx, "twse", "FMTQIK", day(2026, 8, 7), []byte(`{"v":0}`)); err != nil {
		t.Fatalf("寫入前一交易日: %v", err)
	}

	got, err := s.ListRawPayloads(ctx, "twse", "FMTQIK")
	if err != nil {
		t.Fatalf("ListRawPayloads: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("raw 列數 = %d, want 2（同 key 覆寫不長列）", len(got))
	}
	if !got[0].TradeDate.Equal(day(2026, 8, 7)) {
		t.Errorf("第一列 = %v, want 8/7（reparse 要照時間順序重放）", got[0].TradeDate)
	}
	// 存進 JSONB 再讀出來會被正規化（空白、鍵序），比字串沒有意義，解開比值。
	var payload struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal(got[1].Payload, &payload); err != nil {
		t.Fatalf("payload 不是合法 JSON: %v (%s)", err, got[1].Payload)
	}
	if payload.V != 2 {
		t.Errorf("payload.v = %d, want 2（第二次寫入的值）", payload.V)
	}
}

// ---------- watchlist（seed 檔重跑）----------

func TestUpsertWatch_IsIdempotentSoSeedCanRerun(t *testing.T) {
	s, ctx := newStore(t)
	bought := day(2026, 3, 14)

	w := store.WatchItem{Symbol: "2330", Shares: 1000, AvgCost: f(950.5), BoughtAt: &bought, Note: "核心持股"}
	if err := s.UpsertWatch(ctx, w); err != nil {
		t.Fatalf("第一次 UpsertWatch: %v", err)
	}
	w.Shares = 2000 // 加碼後改檔案重跑 seed
	w.AvgCost = f(980)
	if err := s.UpsertWatch(ctx, w); err != nil {
		t.Fatalf("第二次 UpsertWatch: %v", err)
	}

	list, err := s.ListWatchlist(ctx)
	if err != nil {
		t.Fatalf("ListWatchlist: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("watchlist 筆數 = %d, want 1（同代號覆寫不長列）", len(list))
	}
	if list[0].Shares != 2000 {
		t.Errorf("Shares = %d, want 2000", list[0].Shares)
	}
	if list[0].AvgCost == nil || *list[0].AvgCost != 980 {
		t.Errorf("AvgCost = %v, want 980", list[0].AvgCost)
	}
	if list[0].BoughtAt == nil || !list[0].BoughtAt.Equal(bought) {
		t.Errorf("BoughtAt = %v, want %v", list[0].BoughtAt, bought)
	}
}

func TestRemoveWatch_ReportsWhetherAnythingWasDeleted(t *testing.T) {
	s, ctx := newStore(t)

	if err := s.UpsertWatch(ctx, store.WatchItem{Symbol: "2330", Shares: 0}); err != nil {
		t.Fatalf("UpsertWatch: %v", err)
	}

	removed, err := s.RemoveWatch(ctx, "2330")
	if err != nil {
		t.Fatalf("RemoveWatch: %v", err)
	}
	if !removed {
		t.Error("removed = false, want true（清單裡有這檔）")
	}

	removed, err = s.RemoveWatch(ctx, "2330")
	if err != nil {
		t.Fatalf("重複 RemoveWatch 不該是錯誤: %v", err)
	}
	if removed {
		t.Error("removed = true, want false（本來就不在清單裡不是錯誤，但要回報沒刪到）")
	}
}

// ---------- 新聞去重 ----------

// news job 每 30 分跑一次，同一則新聞會重複出現在 RSS 裡。
func TestInsertNews_DeduplicatesByURL(t *testing.T) {
	s, ctx := newStore(t)

	n := store.NewsItem{
		Source: "ltn-rss", URL: "https://example.com/a", Title: "台積電法說",
		Body: "內文", PublishedAt: day(2026, 8, 10), Symbols: []string{"2330"},
	}

	inserted, err := s.InsertNews(ctx, n)
	if err != nil {
		t.Fatalf("第一次 InsertNews: %v", err)
	}
	if !inserted {
		t.Error("inserted = false, want true（第一次應該真的寫入）")
	}

	n.Title = "標題被改了" // 同 url 就是同一則，不覆寫
	inserted, err = s.InsertNews(ctx, n)
	if err != nil {
		t.Fatalf("第二次 InsertNews: %v", err)
	}
	if inserted {
		t.Error("inserted = true, want false（同 url 應被去重）")
	}
	if got := countRows(t, ctx, "news"); got != 1 {
		t.Errorf("news 列數 = %d, want 1", got)
	}
}

// ---------- 摘要 ----------

// summary --date 重生成要蓋掉舊的，不能同日同 model 留兩份。
func TestSaveSummary_OverwritesSameDateProviderModel(t *testing.T) {
	s, ctx := newStore(t)
	d := day(2026, 8, 10)

	if err := s.SaveSummary(ctx, store.Summary{TradeDate: d, Provider: "gemini", Model: "gemini-2.5-flash", Content: "舊版"}); err != nil {
		t.Fatalf("第一次 SaveSummary: %v", err)
	}
	if err := s.SaveSummary(ctx, store.Summary{TradeDate: d, Provider: "gemini", Model: "gemini-2.5-flash", Content: "新版"}); err != nil {
		t.Fatalf("第二次 SaveSummary: %v", err)
	}
	// 換 model 是不同一列（要能比較兩個模型的產出）。
	if err := s.SaveSummary(ctx, store.Summary{TradeDate: d, Provider: "anthropic", Model: "claude", Content: "另一個模型"}); err != nil {
		t.Fatalf("換 provider 的 SaveSummary: %v", err)
	}

	if n := countRows(t, ctx, "summaries"); n != 2 {
		t.Errorf("summaries 列數 = %d, want 2（同 key 覆寫、換 model 另存）", n)
	}
	got, err := s.GetSummary(ctx, d)
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if got.Content != "另一個模型" && got.Content != "新版" {
		t.Errorf("Content = %q, 不是任何一次寫入的內容", got.Content)
	}
}

// ---------- eval run 原子性 ----------

func TestSaveEvalRun_WritesRunAndCases(t *testing.T) {
	s, ctx := newStore(t)

	runID, err := s.SaveEvalRun(ctx, "gemini", "gemini-2.5-flash", []store.EvalCaseResult{
		{CaseName: "外資買超方向", Passed: true},
		{CaseName: "不得出現建議字眼", Passed: false, Detail: "出現「建議買進」"},
	})
	if err != nil {
		t.Fatalf("SaveEvalRun: %v", err)
	}
	if runID == 0 {
		t.Error("runID = 0, want 非零")
	}
	if n := countRows(t, ctx, "eval_runs"); n != 1 {
		t.Errorf("eval_runs 列數 = %d, want 1", n)
	}
	if n := countRows(t, ctx, "eval_case_results"); n != 2 {
		t.Errorf("eval_case_results 列數 = %d, want 2", n)
	}
}

// 明細寫到一半失敗必須整包回滾——不能留「有 header 沒明細」的半套 run，
// 那會讓 pass rate 統計失真而且看不出來。
// 用 NUL byte 觸發 pg 的 text 編碼錯誤，模擬第二筆明細寫入失敗。
func TestSaveEvalRun_RollsBackWholeRunWhenACaseFails(t *testing.T) {
	s, ctx := newStore(t)

	_, err := s.SaveEvalRun(ctx, "gemini", "gemini-2.5-flash", []store.EvalCaseResult{
		{CaseName: "正常案例", Passed: true},
		{CaseName: "壞案例\x00", Passed: true},
	})
	if err == nil {
		t.Fatal("SaveEvalRun = nil error, want 錯誤（第二筆明細應寫入失敗）")
	}
	if n := countRows(t, ctx, "eval_runs"); n != 0 {
		t.Errorf("eval_runs 列數 = %d, want 0（明細失敗時 run header 必須一起回滾）", n)
	}
	if n := countRows(t, ctx, "eval_case_results"); n != 0 {
		t.Errorf("eval_case_results 列數 = %d, want 0", n)
	}
}
