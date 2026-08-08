// Package infra 接 rediskit（姊妹專案 go-redis-kit）：排程分散式鎖 + 出站限速。
// Redis 未設定時全部退化成 no-op——單機開發不用先起 Redis。
package infra

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/brucechen520/go-redis-kit/rediskit"
)

// Infra 聚合 rediskit client 與衍生的 lock / limiter。
type Infra struct {
	client *rediskit.Client // nil = Redis 停用
}

// New 建立 Infra。addr 空字串 = 停用（全 no-op）。
func New(addr, password string) (*Infra, error) {
	if addr == "" {
		return &Infra{}, nil
	}
	c, err := rediskit.New(
		rediskit.WithAddr(addr),
		rediskit.WithPassword(password),
		rediskit.WithNamespace("dstock"),
	)
	if err != nil {
		return nil, err
	}
	return &Infra{client: c}, nil
}

func (i *Infra) Close() {
	if i.client != nil {
		_ = i.client.Close()
	}
}

// WithLock 以分散式鎖包一個 job：鎖被別人持有 → 跳過（多實例防重抓）；
// Redis 掛掉 → 照常執行 + 警告（單機場景鎖只是保險，不因保險壞掉就不幹活）。
func (i *Infra) WithLock(ctx context.Context, name string, ttl time.Duration, fn func(ctx context.Context) error) error {
	if i.client == nil {
		return fn(ctx)
	}
	lock, err := i.client.Locker().Obtain(ctx, name, ttl)
	if errors.Is(err, rediskit.ErrLockNotObtained) {
		log.Printf("[lock] %s 已被其他實例持有，跳過", name)
		return nil
	}
	if err != nil {
		log.Printf("[lock] %s 取鎖失敗（%v），無鎖執行", name, err)
		return fn(ctx)
	}
	defer func() {
		if rerr := lock.Release(ctx); rerr != nil {
			log.Printf("[lock] %s 釋放異常：%v", name, rerr)
		}
	}()
	return fn(ctx)
}

// OutboundLimiter 回傳出站限速器（market.Limiter 介面）。
// 對 TWSE 3 req/s——太快會被 ban，整條管線停擺。
func (i *Infra) OutboundLimiter() *WaitLimiter {
	if i.client == nil {
		return &WaitLimiter{} // no-op
	}
	return &WaitLimiter{rl: i.client.RateLimiter(3, time.Second)}
}

// WaitLimiter 把 rediskit 的「立即拒絕」語意轉成爬蟲要的「等到可以為止」。
type WaitLimiter struct {
	rl *rediskit.RateLimiter
}

func (w *WaitLimiter) Wait(ctx context.Context) error {
	if w.rl == nil {
		return nil
	}
	for {
		err := w.rl.Allow(ctx, "twse")
		if err == nil {
			return nil
		}
		if !errors.Is(err, rediskit.ErrRateLimited) {
			log.Printf("[limiter] redis 異常（%v），本次不限速", err)
			return nil // 限速器壞了不該讓爬蟲停擺；被 ban 的風險自負，log 看得到
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
