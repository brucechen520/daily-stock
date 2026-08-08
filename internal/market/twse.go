package market

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Limiter 是出站限速的抽象（rediskit RateLimiter 或 no-op）。
// 對 TWSE 太快會被 ban，整條管線停擺——所有請求都要過它。
type Limiter interface {
	Wait(ctx context.Context) error
}

// NopLimiter 不限速（測試 / 未設 Redis 時用）。
type NopLimiter struct{}

func (NopLimiter) Wait(context.Context) error { return nil }

// TWSEClient 抓 TWSE 兩個來源：
//   - openapi.twse.com.tw（FMTQIK / STOCK_DAY_ALL，只回最新資料）
//   - www.twse.com.tw/rwd（BFI82U / STOCK_DAY，可帶日期查歷史 → backfill 用）
type TWSEClient struct {
	HTTP        *http.Client
	Limiter     Limiter
	OpenAPIBase string // 測試注入 httptest server
	RWDBase     string
}

func NewTWSEClient(l Limiter) *TWSEClient {
	if l == nil {
		l = NopLimiter{}
	}
	return &TWSEClient{
		HTTP:        &http.Client{Timeout: 30 * time.Second},
		Limiter:     l,
		OpenAPIBase: "https://openapi.twse.com.tw/v1",
		RWDBase:     "https://www.twse.com.tw",
	}
}

func (c *TWSEClient) get(ctx context.Context, url string) ([]byte, error) {
	if err := c.Limiter.Wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// 表明身分（版權紀律）
	req.Header.Set("User-Agent", "daily-stock/0.1 (personal research; github.com/brucechen520/daily-stock)")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

// FetchIndexMonth 抓當月大盤每日成交（FMTQIK）。回傳 (解析結果, 原始 payload)。
// 一次拿整月是免費的補漏機制——某天排程失敗，隔天照樣把整月 upsert 回來。
func (c *TWSEClient) FetchIndexMonth(ctx context.Context) ([]IndexDaily, []byte, error) {
	raw, err := c.get(ctx, c.OpenAPIBase+"/exchangeReport/FMTQIK")
	if err != nil {
		return nil, nil, err
	}
	rows, err := ParseFMTQIK(raw)
	return rows, raw, err
}

// FetchInstitutional 抓指定日的三大法人統計（BFI82U，可查歷史）。
// 非交易日回 (nil, raw, nil)。
func (c *TWSEClient) FetchInstitutional(ctx context.Context, date time.Time) ([]Institutional, []byte, error) {
	url := fmt.Sprintf("%s/rwd/zh/fund/BFI82U?dayDate=%s&type=day&response=json",
		c.RWDBase, date.Format("20060102"))
	raw, err := c.get(ctx, url)
	if err != nil {
		return nil, nil, err
	}
	rows, err := ParseBFI82U(raw, date)
	return rows, raw, err
}

// FetchAllStockDaily 抓最新交易日全市場個股日成交（STOCK_DAY_ALL）。
func (c *TWSEClient) FetchAllStockDaily(ctx context.Context) ([]StockDaily, []byte, error) {
	raw, err := c.get(ctx, c.OpenAPIBase+"/exchangeReport/STOCK_DAY_ALL")
	if err != nil {
		return nil, nil, err
	}
	rows, err := ParseStockDayAll(raw)
	return rows, raw, err
}

// FetchStockMonth 抓某股某月的每日成交（STOCK_DAY，backfill 用）。
// month 用該月任一天表示（通常給 1 號）。
func (c *TWSEClient) FetchStockMonth(ctx context.Context, symbol string, month time.Time) ([]StockDaily, []byte, error) {
	url := fmt.Sprintf("%s/exchangeReport/STOCK_DAY?response=json&date=%s&stockNo=%s",
		c.RWDBase, month.Format("20060102"), symbol)
	raw, err := c.get(ctx, url)
	if err != nil {
		return nil, nil, err
	}
	rows, err := ParseStockDayMonth(raw, symbol)
	return rows, raw, err
}
