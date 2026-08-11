package indicator_test

import (
	"testing"
	"time"

	"github.com/brucechen520/daily-stock/internal/indicator"
)

func day(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

func f(v float64) *float64 { return &v }

// bars 由新到舊（store 的查詢順序），與實際使用一致。
func bars() []indicator.Bar {
	return []indicator.Bar{
		{TradeDate: day(2026, 8, 10), Close: f(1100), Volume: 30_000_000},
		{TradeDate: day(2026, 8, 7), Close: f(1000), Volume: 20_000_000},
		{TradeDate: day(2026, 8, 6), Close: f(980), Volume: 10_000_000},
	}
}

func TestDaily_ComputesChangeAgainstPreviousBar(t *testing.T) {
	got, err := indicator.Daily(bars())

	if err != nil {
		t.Fatalf("Daily 回傳非預期錯誤: %v", err)
	}
	if got.Close != 1100 {
		t.Errorf("Close = %v, want 1100（最新一筆）", got.Close)
	}
	if want := 100.0; got.Change != want {
		t.Errorf("Change = %v, want %v", got.Change, want)
	}
	if want := 10.0; got.ChangePct != want {
		t.Errorf("ChangePct = %v, want %v（1000 → 1100）", got.ChangePct, want)
	}
}

func TestDaily_NegativeChange(t *testing.T) {
	got, err := indicator.Daily([]indicator.Bar{
		{TradeDate: day(2026, 8, 10), Close: f(90)},
		{TradeDate: day(2026, 8, 7), Close: f(100)},
	})

	if err != nil {
		t.Fatalf("Daily 回傳非預期錯誤: %v", err)
	}
	if want := -10.0; got.ChangePct != want {
		t.Errorf("ChangePct = %v, want %v", got.ChangePct, want)
	}
}

// 只有一根 K（剛加入 watchlist、還沒回補）時算不出漲跌幅，但不該讓整份推播掛掉。
func TestDaily_SingleBarHasNoChange(t *testing.T) {
	got, err := indicator.Daily([]indicator.Bar{
		{TradeDate: day(2026, 8, 10), Close: f(50)},
	})

	if err != nil {
		t.Fatalf("Daily 回傳非預期錯誤: %v", err)
	}
	if got.Close != 50 {
		t.Errorf("Close = %v, want 50", got.Close)
	}
	if got.HasChange {
		t.Error("只有一根 K 時 HasChange 應為 false")
	}
}

func TestDaily_EmptyBarsIsError(t *testing.T) {
	_, err := indicator.Daily(nil)

	if err == nil {
		t.Error("Daily(nil) = nil error, want 錯誤（沒有任何資料是呼叫端的問題）")
	}
}

// TWSE 對當天無成交的股票回 "--"，store 存成 NULL。這種列不能當有效收盤價。
func TestDaily_SkipsNullCloseBars(t *testing.T) {
	got, err := indicator.Daily([]indicator.Bar{
		{TradeDate: day(2026, 8, 10), Close: nil},
		{TradeDate: day(2026, 8, 7), Close: f(200)},
		{TradeDate: day(2026, 8, 6), Close: f(190)},
	})

	if err != nil {
		t.Fatalf("Daily 回傳非預期錯誤: %v", err)
	}
	if got.Close != 200 {
		t.Errorf("Close = %v, want 200（跳過 NULL 那筆）", got.Close)
	}
	if want := 190.0; got.PrevClose != want {
		t.Errorf("PrevClose = %v, want %v", got.PrevClose, want)
	}
}

func TestDaily_AllNullClosesIsError(t *testing.T) {
	_, err := indicator.Daily([]indicator.Bar{
		{TradeDate: day(2026, 8, 10), Close: nil},
		{TradeDate: day(2026, 8, 7), Close: nil},
	})

	if err == nil {
		t.Error("全部無成交應回錯誤")
	}
}
