// Package ingest 是「抓 → 落地」的編排層：market/news 抓，store 存。
// 全部冪等：同日重跑任意次結果相同。
package ingest

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/brucechen520/daily-stock/internal/market"
	"github.com/brucechen520/daily-stock/internal/news"
	"github.com/brucechen520/daily-stock/internal/store"
)

type Service struct {
	TWSE  *market.TWSEClient
	News  *news.Fetcher
	Store *store.Store
}

// IngestMarket 抓大盤（整月）+ 三大法人 + 全市場日K，raw 與 normalized 都落地。
// 非交易日（法人查無資料）不算失敗。
func (s *Service) IngestMarket(ctx context.Context) error {
	// 1. 大盤：FMTQIK 回整月 → 免費補漏
	idxRows, idxRaw, err := s.TWSE.FetchIndexMonth(ctx)
	if err != nil {
		return fmt.Errorf("ingest index: %w", err)
	}
	if len(idxRows) == 0 {
		log.Println("[ingest] FMTQIK 無資料（月初非交易日？）")
		return nil
	}
	latest := idxRows[len(idxRows)-1].TradeDate
	if err := s.Store.UpsertRawPayload(ctx, "twse", "FMTQIK", latest, idxRaw); err != nil {
		return err
	}
	if err := s.Store.UpsertIndexDaily(ctx, toStoreIndex(idxRows)); err != nil {
		return err
	}

	// 2. 三大法人：查最新交易日
	instRows, instRaw, err := s.TWSE.FetchInstitutional(ctx, latest)
	if err != nil {
		return fmt.Errorf("ingest institutional: %w", err)
	}
	if err := s.Store.UpsertRawPayload(ctx, "twse", "BFI82U", latest, instRaw); err != nil {
		return err
	}
	if instRows != nil {
		if err := s.Store.UpsertInstitutional(ctx, toStoreInst(instRows)); err != nil {
			return err
		}
	}

	// 3. 全市場個股日K（自帶日期）
	stockRows, stockRaw, err := s.TWSE.FetchAllStockDaily(ctx)
	if err != nil {
		return fmt.Errorf("ingest stocks: %w", err)
	}
	if len(stockRows) > 0 {
		if err := s.Store.UpsertRawPayload(ctx, "twse", "STOCK_DAY_ALL", stockRows[0].TradeDate, stockRaw); err != nil {
			return err
		}
		if err := s.Store.UpsertStockDaily(ctx, toStoreStock(stockRows)); err != nil {
			return err
		}
	}
	log.Printf("[ingest] market 完成：index=%d 日, institutional=%v, stocks=%d 檔（交易日 %s）",
		len(idxRows), instRows != nil, len(stockRows), latest.Format("2006-01-02"))
	return nil
}

// IngestNews 抓 RSS 落地（url 去重）。部分 feed 失敗不擋整體。
func (s *Service) IngestNews(ctx context.Context) error {
	items, ferr := s.News.FetchAll(ctx) // ferr 可能是部分失敗，先存成功的
	inserted := 0
	for _, it := range items {
		ok, err := s.Store.InsertNews(ctx, store.NewsItem{
			Source: it.Source, URL: it.URL, Title: it.Title, Body: it.Body,
			PublishedAt: it.PublishedAt, Symbols: it.Symbols,
		})
		if err != nil {
			return fmt.Errorf("insert news: %w", err)
		}
		if ok {
			inserted++
		}
	}
	log.Printf("[ingest] news 完成：抓 %d 則、新增 %d 則", len(items), inserted)
	return ferr
}

// Backfill 回補個股歷史（按月分頁），from/to 格式 YYYY-MM。
func (s *Service) Backfill(ctx context.Context, symbol string, from, to time.Time) error {
	total := 0
	for m := from; !m.After(to); m = m.AddDate(0, 1, 0) {
		rows, raw, err := s.TWSE.FetchStockMonth(ctx, symbol, m)
		if err != nil {
			return fmt.Errorf("backfill %s %s: %w", symbol, m.Format("2006-01"), err)
		}
		endpoint := "STOCK_DAY/" + symbol
		if err := s.Store.UpsertRawPayload(ctx, "twse", endpoint, m, raw); err != nil {
			return err
		}
		if err := s.Store.UpsertStockDaily(ctx, toStoreStock(rows)); err != nil {
			return err
		}
		total += len(rows)
		log.Printf("[backfill] %s %s：%d 日", symbol, m.Format("2006-01"), len(rows))
	}
	log.Printf("[backfill] %s 完成，共 %d 日", symbol, total)
	return nil
}

// Reparse 重放 raw_payloads：解析 bug 修完跑這個，不用重爬。
func (s *Service) Reparse(ctx context.Context) error {
	// FMTQIK
	raws, err := s.Store.ListRawPayloads(ctx, "twse", "FMTQIK")
	if err != nil {
		return err
	}
	for _, r := range raws {
		rows, err := market.ParseFMTQIK(r.Payload)
		if err != nil {
			return fmt.Errorf("reparse FMTQIK %s: %w", r.TradeDate.Format("2006-01-02"), err)
		}
		if err := s.Store.UpsertIndexDaily(ctx, toStoreIndex(rows)); err != nil {
			return err
		}
	}
	// BFI82U
	raws, err = s.Store.ListRawPayloads(ctx, "twse", "BFI82U")
	if err != nil {
		return err
	}
	for _, r := range raws {
		rows, err := market.ParseBFI82U(r.Payload, r.TradeDate)
		if err != nil {
			return fmt.Errorf("reparse BFI82U %s: %w", r.TradeDate.Format("2006-01-02"), err)
		}
		if rows != nil {
			if err := s.Store.UpsertInstitutional(ctx, toStoreInst(rows)); err != nil {
				return err
			}
		}
	}
	// STOCK_DAY_ALL
	raws, err = s.Store.ListRawPayloads(ctx, "twse", "STOCK_DAY_ALL")
	if err != nil {
		return err
	}
	for _, r := range raws {
		rows, err := market.ParseStockDayAll(r.Payload)
		if err != nil {
			return fmt.Errorf("reparse STOCK_DAY_ALL %s: %w", r.TradeDate.Format("2006-01-02"), err)
		}
		if err := s.Store.UpsertStockDaily(ctx, toStoreStock(rows)); err != nil {
			return err
		}
	}
	log.Println("[reparse] 完成")
	return nil
}

// --- market 型別 → store 型別（兩包刻意不互相 import，這裡是唯一轉接點）---

func toStoreIndex(rows []market.IndexDaily) []store.IndexDaily {
	out := make([]store.IndexDaily, len(rows))
	for i, r := range rows {
		out[i] = store.IndexDaily{TradeDate: r.TradeDate, Close: r.Close, Change: r.Change, Volume: r.Volume, Amount: r.Amount}
	}
	return out
}

func toStoreInst(rows []market.Institutional) []store.Institutional {
	out := make([]store.Institutional, len(rows))
	for i, r := range rows {
		out[i] = store.Institutional{TradeDate: r.TradeDate, Actor: r.Actor, BuyAmount: r.BuyAmount, SellAmount: r.SellAmount, NetAmount: r.NetAmount}
	}
	return out
}

func toStoreStock(rows []market.StockDaily) []store.StockDaily {
	out := make([]store.StockDaily, len(rows))
	for i, r := range rows {
		out[i] = store.StockDaily{Symbol: r.Symbol, Name: r.Name, TradeDate: r.TradeDate, Open: r.Open, High: r.High, Low: r.Low, Close: r.Close, Volume: r.Volume}
	}
	return out
}
