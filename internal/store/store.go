// Package store 是唯一的資料存取層：pgx 連線池 + goose migrations + 各表 repository。
// 寫入一律 upsert（ON CONFLICT），同日重跑任意次結果相同——排程重試、手動補抓都安全。
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // goose 走 database/sql，掛 pgx driver
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Store struct {
	pool *pgxpool.Pool
}

// New 建立連線池並跑 migrations（冪等，已套用的版本自動跳過）。
func New(ctx context.Context, dsn string) (*Store, error) {
	if err := migrate(dsn); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return &Store{pool: pool}, nil
}

func migrate(dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.Up(db, "migrations")
}

func (s *Store) Close() { s.pool.Close() }

// ---------- raw 層 ----------

// UpsertRawPayload 落地原始 API 回應。同 (source, endpoint, trade_date) 覆寫。
func (s *Store) UpsertRawPayload(ctx context.Context, source, endpoint string, tradeDate time.Time, payload []byte) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO raw_payloads (source, endpoint, trade_date, payload, fetched_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (source, endpoint, trade_date)
		DO UPDATE SET payload = EXCLUDED.payload, fetched_at = now()`,
		source, endpoint, tradeDate, payload)
	return err
}

// RawPayload 是 reparse 用的原始資料列。
type RawPayload struct {
	Source    string
	Endpoint  string
	TradeDate time.Time
	Payload   []byte
}

// ListRawPayloads 取某 source+endpoint 的全部原始回應（reparse 重放用）。
func (s *Store) ListRawPayloads(ctx context.Context, source, endpoint string) ([]RawPayload, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT source, endpoint, trade_date, payload FROM raw_payloads
		WHERE source = $1 AND endpoint = $2 ORDER BY trade_date`, source, endpoint)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RawPayload
	for rows.Next() {
		var r RawPayload
		if err := rows.Scan(&r.Source, &r.Endpoint, &r.TradeDate, &r.Payload); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------- normalized 層 ----------

type IndexDaily struct {
	TradeDate time.Time
	Close     float64
	Change    float64
	Volume    int64 // 股
	Amount    int64 // 元
}

func (s *Store) UpsertIndexDaily(ctx context.Context, rows []IndexDaily) error {
	for _, r := range rows {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO market_index_daily (trade_date, close, change, volume, amount)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (trade_date)
			DO UPDATE SET close = EXCLUDED.close, change = EXCLUDED.change,
			              volume = EXCLUDED.volume, amount = EXCLUDED.amount`,
			r.TradeDate, r.Close, r.Change, r.Volume, r.Amount); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetIndexDaily(ctx context.Context, date time.Time) (*IndexDaily, error) {
	var r IndexDaily
	err := s.pool.QueryRow(ctx, `
		SELECT trade_date, close, change, volume, amount
		FROM market_index_daily WHERE trade_date = $1`, date).
		Scan(&r.TradeDate, &r.Close, &r.Change, &r.Volume, &r.Amount)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// LatestTradeDate 回傳大盤表裡最新的交易日（summary 預設對象）。
func (s *Store) LatestTradeDate(ctx context.Context) (time.Time, error) {
	var d time.Time
	err := s.pool.QueryRow(ctx, `SELECT max(trade_date) FROM market_index_daily`).Scan(&d)
	return d, err
}

type Institutional struct {
	TradeDate  time.Time
	Actor      string // 'foreign' / 'trust' / 'dealer'
	BuyAmount  int64
	SellAmount int64
	NetAmount  int64
}

func (s *Store) UpsertInstitutional(ctx context.Context, rows []Institutional) error {
	for _, r := range rows {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO institutional_daily (trade_date, actor, buy_amount, sell_amount, net_amount)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (trade_date, actor)
			DO UPDATE SET buy_amount = EXCLUDED.buy_amount, sell_amount = EXCLUDED.sell_amount,
			              net_amount = EXCLUDED.net_amount`,
			r.TradeDate, r.Actor, r.BuyAmount, r.SellAmount, r.NetAmount); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetInstitutional(ctx context.Context, date time.Time) ([]Institutional, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT trade_date, actor, buy_amount, sell_amount, net_amount
		FROM institutional_daily WHERE trade_date = $1`, date)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Institutional
	for rows.Next() {
		var r Institutional
		if err := rows.Scan(&r.TradeDate, &r.Actor, &r.BuyAmount, &r.SellAmount, &r.NetAmount); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type StockDaily struct {
	Symbol    string
	Name      string
	TradeDate time.Time
	Open      *float64 // 無成交時 TWSE 回 "--" → NULL
	High      *float64
	Low       *float64
	Close     *float64
	Volume    int64
}

func (s *Store) UpsertStockDaily(ctx context.Context, rows []StockDaily) error {
	// 全市場一天 ~2000 筆，逐筆 upsert 在本機 pg 足夠快；之後量大再換 COPY。
	for _, r := range rows {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO stock_daily (symbol, name, trade_date, open, high, low, close, volume)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (symbol, trade_date)
			DO UPDATE SET name = EXCLUDED.name, open = EXCLUDED.open, high = EXCLUDED.high,
			              low = EXCLUDED.low, close = EXCLUDED.close, volume = EXCLUDED.volume`,
			r.Symbol, r.Name, r.TradeDate, r.Open, r.High, r.Low, r.Close, r.Volume); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 新聞 ----------

type NewsItem struct {
	Source      string
	URL         string
	Title       string
	Body        string
	PublishedAt time.Time
	Symbols     []string
}

// InsertNews 以 url 去重；已存在回 false。
func (s *Store) InsertNews(ctx context.Context, n NewsItem) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO news (source, url, title, body, published_at, symbols)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (url) DO NOTHING`,
		n.Source, n.URL, n.Title, n.Body, n.PublishedAt, n.Symbols)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ---------- 摘要 ----------

type Summary struct {
	TradeDate time.Time
	Provider  string
	Model     string
	Content   string
}

func (s *Store) SaveSummary(ctx context.Context, sm Summary) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO summaries (trade_date, provider, model, content)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (trade_date, provider, model)
		DO UPDATE SET content = EXCLUDED.content, created_at = now()`,
		sm.TradeDate, sm.Provider, sm.Model, sm.Content)
	return err
}

func (s *Store) GetSummary(ctx context.Context, date time.Time) (*Summary, error) {
	var sm Summary
	err := s.pool.QueryRow(ctx, `
		SELECT trade_date, provider, model, content FROM summaries
		WHERE trade_date = $1 ORDER BY created_at DESC LIMIT 1`, date).
		Scan(&sm.TradeDate, &sm.Provider, &sm.Model, &sm.Content)
	if err != nil {
		return nil, err
	}
	return &sm, nil
}

// ---------- eval ----------

type EvalCaseResult struct {
	CaseName string
	Passed   bool
	Detail   string
}

func (s *Store) SaveEvalRun(ctx context.Context, provider, model string, results []EvalCaseResult) (int64, error) {
	passed := 0
	for _, r := range results {
		if r.Passed {
			passed++
		}
	}
	var runID int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO eval_runs (provider, model, total, passed)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		provider, model, len(results), passed).Scan(&runID)
	if err != nil {
		return 0, err
	}
	for _, r := range results {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO eval_case_results (run_id, case_name, passed, detail)
			VALUES ($1, $2, $3, $4)`, runID, r.CaseName, r.Passed, r.Detail); err != nil {
			return 0, err
		}
	}
	return runID, nil
}
