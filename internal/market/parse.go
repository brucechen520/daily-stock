// Package market 是 TWSE 爬蟲層：抓原始回應 + 解析成 normalized 結構。
// 解析函式全是純函式（raw []byte 進、結構出），fetcher 與 reparse 共用同一套——
// 解析 bug 修完重放 raw_payloads 即可，不用重爬。
package market

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// IndexDaily / Institutional / StockDaily 對齊 store 的同名結構（market 不 import store，保持單向依賴）。
type IndexDaily struct {
	TradeDate time.Time
	Close     float64
	Change    float64
	Volume    int64
	Amount    int64
}

type Institutional struct {
	TradeDate  time.Time
	Actor      string // 'foreign' / 'trust' / 'dealer'
	BuyAmount  int64
	SellAmount int64
	NetAmount  int64
}

type StockDaily struct {
	Symbol    string
	Name      string
	TradeDate time.Time
	Open      *float64
	High      *float64
	Low       *float64
	Close     *float64
	Volume    int64
}

// parseROCDate 解析 TWSE 民國年日期。兩種格式都出現過：
//   - "1150803"（OpenAPI：民國115年8月3日）
//   - "115/08/03"（www.twse.com.tw 的 STOCK_DAY）
func parseROCDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	var y, m, d int
	if strings.Contains(s, "/") {
		parts := strings.Split(s, "/")
		if len(parts) != 3 {
			return time.Time{}, fmt.Errorf("bad roc date %q", s)
		}
		y, _ = strconv.Atoi(parts[0])
		m, _ = strconv.Atoi(parts[1])
		d, _ = strconv.Atoi(parts[2])
	} else {
		if len(s) != 7 {
			return time.Time{}, fmt.Errorf("bad roc date %q", s)
		}
		y, _ = strconv.Atoi(s[:3])
		m, _ = strconv.Atoi(s[3:5])
		d, _ = strconv.Atoi(s[5:7])
	}
	if y == 0 || m == 0 || d == 0 {
		return time.Time{}, fmt.Errorf("bad roc date %q", s)
	}
	return time.Date(y+1911, time.Month(m), d, 0, 0, 0, 0, time.UTC), nil
}

// parseNum 解析 TWSE 數字字串：可能帶千分位逗號、正負號；"--" 與空字串代表無值。
func parseNum(s string) (float64, bool) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	s = strings.TrimPrefix(s, "+")
	if s == "" || s == "--" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func parseInt(s string) int64 {
	f, _ := parseNum(s)
	return int64(f)
}

// ---------- FMTQIK：每日市場成交資訊（大盤） ----------

type fmtqikRow struct {
	Date        string `json:"Date"`
	TradeVolume string `json:"TradeVolume"`
	TradeValue  string `json:"TradeValue"`
	TAIEX       string `json:"TAIEX"`
	Change      string `json:"Change"`
}

// ParseFMTQIK 解析 OpenAPI FMTQIK（回當月每個交易日一列）。
func ParseFMTQIK(raw []byte) ([]IndexDaily, error) {
	var rows []fmtqikRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("fmtqik: %w", err)
	}
	var out []IndexDaily
	for _, r := range rows {
		d, err := parseROCDate(r.Date)
		if err != nil {
			return nil, fmt.Errorf("fmtqik row: %w", err)
		}
		closeV, ok := parseNum(r.TAIEX)
		if !ok {
			continue
		}
		change, _ := parseNum(r.Change)
		out = append(out, IndexDaily{
			TradeDate: d,
			Close:     closeV,
			Change:    change,
			Volume:    parseInt(r.TradeVolume),
			Amount:    parseInt(r.TradeValue),
		})
	}
	return out, nil
}

// ---------- BFI82U：三大法人買賣金額統計 ----------

type bfi82uResp struct {
	Stat string     `json:"stat"`
	Data [][]string `json:"data"` // [單位名稱, 買進金額, 賣出金額, 買賣差額]
}

// ParseBFI82U 解析 www.twse.com.tw/rwd 版三大法人統計，聚合成三個 actor：
//
//	foreign = 外資及陸資(不含外資自營商) + 外資自營商
//	trust   = 投信
//	dealer  = 自營商(自行買賣) + 自營商(避險)
//
// 非交易日 stat != "OK"，回 (nil, nil)——是「無資料」不是錯誤。
func ParseBFI82U(raw []byte, tradeDate time.Time) ([]Institutional, error) {
	var resp bfi82uResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("bfi82u: %w", err)
	}
	if resp.Stat != "OK" {
		return nil, nil // 非交易日
	}
	agg := map[string]*Institutional{
		"foreign": {TradeDate: tradeDate, Actor: "foreign"},
		"trust":   {TradeDate: tradeDate, Actor: "trust"},
		"dealer":  {TradeDate: tradeDate, Actor: "dealer"},
	}
	actorOf := func(name string) string {
		switch {
		case strings.HasPrefix(name, "外資"): // 外資及陸資 / 外資自營商
			return "foreign"
		case name == "投信":
			return "trust"
		case strings.HasPrefix(name, "自營商"):
			return "dealer"
		default: // 合計列（可由三者推導）不入庫
			return ""
		}
	}
	for _, row := range resp.Data {
		if len(row) < 4 {
			continue
		}
		key := actorOf(strings.TrimSpace(row[0]))
		if key == "" {
			continue
		}
		agg[key].BuyAmount += parseInt(row[1])
		agg[key].SellAmount += parseInt(row[2])
		agg[key].NetAmount += parseInt(row[3])
	}
	return []Institutional{*agg["foreign"], *agg["trust"], *agg["dealer"]}, nil
}

// ---------- STOCK_DAY_ALL：全市場個股日成交 ----------

type stockDayAllRow struct {
	Date         string `json:"Date"`
	Code         string `json:"Code"`
	Name         string `json:"Name"`
	TradeVolume  string `json:"TradeVolume"`
	OpeningPrice string `json:"OpeningPrice"`
	HighestPrice string `json:"HighestPrice"`
	LowestPrice  string `json:"LowestPrice"`
	ClosingPrice string `json:"ClosingPrice"`
}

// ParseStockDayAll 解析 OpenAPI STOCK_DAY_ALL（最新交易日、全市場一次回）。
func ParseStockDayAll(raw []byte) ([]StockDaily, error) {
	var rows []stockDayAllRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("stock_day_all: %w", err)
	}
	out := make([]StockDaily, 0, len(rows))
	for _, r := range rows {
		d, err := parseROCDate(r.Date)
		if err != nil {
			continue // 個別壞列跳過，不炸整批
		}
		out = append(out, StockDaily{
			Symbol:    strings.TrimSpace(r.Code),
			Name:      strings.TrimSpace(r.Name),
			TradeDate: d,
			Open:      numPtr(r.OpeningPrice),
			High:      numPtr(r.HighestPrice),
			Low:       numPtr(r.LowestPrice),
			Close:     numPtr(r.ClosingPrice),
			Volume:    parseInt(r.TradeVolume),
		})
	}
	return out, nil
}

// ---------- STOCK_DAY：個股歷史月資料（backfill 用） ----------

type stockDayResp struct {
	Stat string     `json:"stat"`
	Data [][]string `json:"data"` // [日期, 成交股數, 成交金額, 開, 高, 低, 收, 漲跌, 筆數, 註記]
}

// ParseStockDayMonth 解析 www.twse.com.tw 的 STOCK_DAY（某股某月每日一列）。
func ParseStockDayMonth(raw []byte, symbol string) ([]StockDaily, error) {
	var resp stockDayResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("stock_day: %w", err)
	}
	if resp.Stat != "OK" {
		return nil, nil // 該月無資料（未上市等）
	}
	var out []StockDaily
	for _, row := range resp.Data {
		if len(row) < 7 {
			continue
		}
		d, err := parseROCDate(row[0])
		if err != nil {
			continue
		}
		out = append(out, StockDaily{
			Symbol:    symbol,
			TradeDate: d,
			Volume:    parseInt(row[1]),
			Open:      numPtr(row[3]),
			High:      numPtr(row[4]),
			Low:       numPtr(row[5]),
			Close:     numPtr(row[6]),
		})
	}
	return out, nil
}

func numPtr(s string) *float64 {
	f, ok := parseNum(s)
	if !ok {
		return nil
	}
	return &f
}
