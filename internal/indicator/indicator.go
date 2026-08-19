// Package indicator 從個股日K序列算出描述性統計。
//
// 明確的界線：這裡只做「把公開數字換算成人看得懂的形式」——漲跌幅、量能比、
// 均線位置。不打分、不分級、不出訊號。訊號要等 walk-forward 回測站得住
// （ROADMAP 原則 4），那是 phase 2' 的事。
//
// Phase 1.5 週 1 只做當日漲跌幅；MA / 量能比 / 距高低點 / 連買連賣在週 2 補。
package indicator

import (
	"errors"
	"time"
)

// Bar 是一根日K。Close 為指標，因為 TWSE 對當天無成交的股票回 "--"（store 存 NULL）。
type Bar struct {
	TradeDate time.Time
	Close     *float64
	Volume    int64
}

// DailyStat 是單一個股的當日統計。
type DailyStat struct {
	TradeDate time.Time
	Close     float64
	PrevClose float64
	Change    float64
	ChangePct float64
	Volume    int64
	// HasChange 為 false 代表只有一根有效K（剛加入 watchlist、還沒回補），
	// 算不出漲跌幅。呼叫端該顯示「—」而不是 0%。
	HasChange bool
}

// ErrNoData 表示連一根有成交的K都沒有。
var ErrNoData = errors.New("indicator: 沒有可用的收盤價")

// Daily 取最新兩根「有成交」的K算當日漲跌幅。
// bars 需由新到舊排序（與 store.RecentStockDaily 的回傳一致）。
func Daily(bars []Bar) (DailyStat, error) {
	valid := make([]Bar, 0, 2)
	for _, b := range bars {
		if b.Close != nil {
			valid = append(valid, b)
			if len(valid) == 2 {
				break
			}
		}
	}
	if len(valid) == 0 {
		return DailyStat{}, ErrNoData
	}

	latest := valid[0]
	stat := DailyStat{
		TradeDate: latest.TradeDate,
		Close:     *latest.Close,
		Volume:    latest.Volume,
	}
	if len(valid) < 2 {
		return stat, nil
	}

	prev := *valid[1].Close
	stat.PrevClose = prev
	stat.Change = stat.Close - prev
	stat.HasChange = true
	if prev != 0 {
		stat.ChangePct = stat.Change / prev * 100
	}
	return stat, nil
}
