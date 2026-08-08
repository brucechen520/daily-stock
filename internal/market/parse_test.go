package market_test

import (
	"testing"
	"time"

	"github.com/brucechen520/daily-stock/internal/market"
)

// fixture 全部取自 2026-08-07 對 TWSE 的真實回應（截短）。

const fmtqikFixture = `[
{"Date":"1150803","TradeVolume":"11427047935","TradeValue":"885506043091","Transaction":"4191882","TAIEX":"43386.41","Change":"266.66"},
{"Date":"1150806","TradeVolume":"10650583944","TradeValue":"974054973424","Transaction":"4630303","TAIEX":"44396.70","Change":"-214.90"}]`

func TestParseFMTQIK_ParsesROCDateAndNumbers(t *testing.T) {
	rows, err := market.ParseFMTQIK([]byte(fmtqikFixture))

	if err != nil {
		t.Fatalf("ParseFMTQIK 回傳非預期錯誤: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	// 民國 1150803 = 2026-08-03
	if want := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC); !rows[0].TradeDate.Equal(want) {
		t.Errorf("TradeDate = %v, want %v", rows[0].TradeDate, want)
	}
	if want := 43386.41; rows[0].Close != want {
		t.Errorf("Close = %v, want %v", rows[0].Close, want)
	}
	if want := -214.90; rows[1].Change != want {
		t.Errorf("Change = %v, want %v（負值要保留）", rows[1].Change, want)
	}
	if want := int64(11427047935); rows[0].Volume != want {
		t.Errorf("Volume = %d, want %d", rows[0].Volume, want)
	}
}

const bfi82uFixture = `{"stat":"OK","date":"20260806","title":"115年08月06日 三大法人買賣金額統計表",
"fields":["單位名稱","買進金額","賣出金額","買賣差額"],
"data":[
["自營商(自行買賣)","10,711,456,439","10,816,477,933","-105,021,494"],
["自營商(避險)","24,975,808,359","31,183,605,452","-6,207,797,093"],
["投信","23,857,074,616","14,448,687,992","9,408,386,624"],
["外資及陸資(不含外資自營商)","392,402,699,900","390,382,881,760","2,019,818,140"],
["外資自營商","0","0","0"],
["合計","451,947,039,314","446,831,653,137","5,115,386,177"]]}`

func TestParseBFI82U_AggregatesActors(t *testing.T) {
	date := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)

	rows, err := market.ParseBFI82U([]byte(bfi82uFixture), date)

	if err != nil {
		t.Fatalf("ParseBFI82U 回傳非預期錯誤: %v", err)
	}
	byActor := map[string]market.Institutional{}
	for _, r := range rows {
		byActor[r.Actor] = r
	}
	// 外資 = 外資及陸資 + 外資自營商（此例外資自營商全 0）
	if want := int64(2_019_818_140); byActor["foreign"].NetAmount != want {
		t.Errorf("foreign net = %d, want %d", byActor["foreign"].NetAmount, want)
	}
	if want := int64(9_408_386_624); byActor["trust"].NetAmount != want {
		t.Errorf("trust net = %d, want %d", byActor["trust"].NetAmount, want)
	}
	// 自營商 = 自行買賣 + 避險：-105,021,494 + -6,207,797,093 = -6,312,818,587
	if want := int64(-6_312_818_587); byActor["dealer"].NetAmount != want {
		t.Errorf("dealer net = %d, want %d", byActor["dealer"].NetAmount, want)
	}
}

func TestParseBFI82U_NonTradingDayReturnsNoRowsNoError(t *testing.T) {
	raw := []byte(`{"stat":"很抱歉，沒有符合條件的資料!"}`)

	rows, err := market.ParseBFI82U(raw, time.Now())

	if err != nil {
		t.Fatalf("非交易日應回 nil error, got %v", err)
	}
	if rows != nil {
		t.Errorf("非交易日應回 nil rows, got %v", rows)
	}
}

const stockDayAllFixture = `[
{"Date":"1150806","Code":"2330","Name":"台積電","TradeVolume":"21186047","TradeValue":"25100000000","OpeningPrice":"1185.00","HighestPrice":"1195.00","LowestPrice":"1180.00","ClosingPrice":"1190.00","Change":"5.0000","Transaction":"35000"},
{"Date":"1150806","Code":"9999","Name":"無成交股","TradeVolume":"0","TradeValue":"0","OpeningPrice":"--","HighestPrice":"--","LowestPrice":"--","ClosingPrice":"--","Change":"0.00","Transaction":"0"}]`

func TestParseStockDayAll_HandlesNoTradePricesAsNull(t *testing.T) {
	rows, err := market.ParseStockDayAll([]byte(stockDayAllFixture))

	if err != nil {
		t.Fatalf("ParseStockDayAll 回傳非預期錯誤: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0].Close == nil || *rows[0].Close != 1190.00 {
		t.Errorf("2330 Close = %v, want 1190.00", rows[0].Close)
	}
	if rows[1].Close != nil {
		t.Errorf("無成交股 Close = %v, want nil（\"--\" 應為 NULL）", *rows[1].Close)
	}
}

const stockDayMonthFixture = `{"stat":"OK","date":"20260701","title":"115年07月 2330 台積電 各日成交資訊",
"fields":["日期","成交股數","成交金額","開盤價","最高價","最低價","收盤價","漲跌價差","成交筆數","註記"],
"data":[
["115/07/01","37,544,470","93,600,076,825","2,495.00","2,505.00","2,475.00","2,505.00","+95.00","111,091",""],
["115/07/02","35,919,290","88,369,879,773","2,450.00","2,480.00","2,445.00","2,465.00","-40.00","132,697",""]]}`

func TestParseStockDayMonth_ParsesSlashROCDateAndCommaNumbers(t *testing.T) {
	rows, err := market.ParseStockDayMonth([]byte(stockDayMonthFixture), "2330")

	if err != nil {
		t.Fatalf("ParseStockDayMonth 回傳非預期錯誤: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if want := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC); !rows[0].TradeDate.Equal(want) {
		t.Errorf("TradeDate = %v, want %v（115/07/01 斜線格式）", rows[0].TradeDate, want)
	}
	if rows[0].Close == nil || *rows[0].Close != 2505.00 {
		t.Errorf("Close = %v, want 2505.00（千分位逗號要去掉）", rows[0].Close)
	}
	if want := int64(37_544_470); rows[0].Volume != want {
		t.Errorf("Volume = %d, want %d", rows[0].Volume, want)
	}
	if rows[0].Symbol != "2330" {
		t.Errorf("Symbol = %q, want \"2330\"", rows[0].Symbol)
	}
}
