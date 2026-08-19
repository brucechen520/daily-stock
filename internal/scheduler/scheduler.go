// Package scheduler 是長駐排程器（docs/phase-1.md §2.3、phase-1.5.md §2.4）：
//
//	平日 17:30  ingest market（失敗重試 3 次、間隔 10 分鐘）
//	平日 18:30  daily digest：生成摘要 + 推播（失敗也會推，見 §4.3）
//	每 30 分鐘  ingest news
//
// 每個 job 都包 rediskit 分散式鎖（多實例防重跑；單機是免費保險）。
package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/brucechen520/daily-stock/internal/infra"
	"github.com/robfig/cron/v3"
)

type Jobs struct {
	IngestMarket func(ctx context.Context) error
	// DailyDigest 生成當日摘要並推播。它自己負責「失敗也要推」，
	// 所以這裡不重試——重試會讓失敗訊息重複洗頻道。
	DailyDigest func(ctx context.Context) error
	IngestNews  func(ctx context.Context) error
}

type Scheduler struct {
	cron  *cron.Cron
	infra *infra.Infra
	jobs  Jobs
}

func New(inf *infra.Infra, jobs Jobs) *Scheduler {
	// 台股時區固定；跑在任何機器上排程時間都對
	loc, err := time.LoadLocation("Asia/Taipei")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	return &Scheduler{
		cron:  cron.New(cron.WithLocation(loc)),
		infra: inf,
		jobs:  jobs,
	}
}

// Run 註冊排程並阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) error {
	add := func(spec, name string, ttl time.Duration, retries int, fn func(context.Context) error) {
		_, err := s.cron.AddFunc(spec, func() {
			jobCtx, cancel := context.WithTimeout(ctx, ttl)
			defer cancel()
			err := s.infra.WithLock(jobCtx, "job:"+name, ttl, func(ctx context.Context) error {
				return runWithRetry(ctx, name, retries, fn)
			})
			if err != nil {
				log.Printf("[schedule] %s 最終失敗：%v", name, err)
			}
		})
		if err != nil {
			panic("bad cron spec " + spec + ": " + err.Error()) // 寫死的 spec，錯了是程式 bug
		}
	}

	// TWSE 盤後資料 ~17:00 完整，17:30 起跑
	add("30 17 * * 1-5", "ingest-market", 40*time.Minute, 3, s.jobs.IngestMarket)
	add("30 18 * * 1-5", "daily-digest", 20*time.Minute, 0, s.jobs.DailyDigest)
	add("*/30 * * * *", "ingest-news", 10*time.Minute, 0, s.jobs.IngestNews)

	log.Println("[schedule] 排程啟動：market 平日 17:30、digest+推播 平日 18:30、news 每 30 分鐘（Asia/Taipei）")
	s.cron.Start()
	<-ctx.Done()
	stopCtx := s.cron.Stop() // 等進行中的 job 跑完
	<-stopCtx.Done()
	return ctx.Err()
}

// runWithRetry 失敗後間隔 10 分鐘重試（ctx 取消即停）。
func runWithRetry(ctx context.Context, name string, retries int, fn func(context.Context) error) error {
	var err error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			log.Printf("[schedule] %s 第 %d 次重試（10 分鐘後）", name, attempt)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Minute):
			}
		}
		if err = fn(ctx); err == nil {
			return nil
		}
		log.Printf("[schedule] %s 失敗：%v", name, err)
	}
	return err
}
