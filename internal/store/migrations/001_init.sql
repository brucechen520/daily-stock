-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;      -- Phase 3 會用，先開

-- raw 層：原始回應全存，解析邏輯改了可以 reparse 重放，不用重新爬
CREATE TABLE raw_payloads (
    id         BIGSERIAL PRIMARY KEY,
    source     TEXT NOT NULL,               -- 'twse' / 'ltn-rss' / ...
    endpoint   TEXT NOT NULL,
    trade_date DATE,
    payload    JSONB NOT NULL,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source, endpoint, trade_date)   -- 冪等鍵：重抓同日覆寫
);

-- normalized 層
CREATE TABLE market_index_daily (
    trade_date DATE PRIMARY KEY,
    close      NUMERIC(10,2) NOT NULL,
    change     NUMERIC(10,2) NOT NULL,
    volume     BIGINT NOT NULL,             -- 成交股數
    amount     BIGINT NOT NULL              -- 成交金額（元）
);

CREATE TABLE institutional_daily (
    trade_date  DATE NOT NULL,
    actor       TEXT NOT NULL,              -- 'foreign' / 'trust' / 'dealer'
    buy_amount  BIGINT NOT NULL,            -- 元
    sell_amount BIGINT NOT NULL,
    net_amount  BIGINT NOT NULL,
    PRIMARY KEY (trade_date, actor)
);

CREATE TABLE stock_daily (
    symbol     TEXT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    trade_date DATE NOT NULL,
    open  NUMERIC(10,2), high NUMERIC(10,2),
    low   NUMERIC(10,2), close NUMERIC(10,2),
    volume BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (symbol, trade_date)
);

-- 新聞（Phase 3 的 news_chunks 會 FK 到這裡）
CREATE TABLE news (
    id           BIGSERIAL PRIMARY KEY,
    source       TEXT NOT NULL,
    url          TEXT NOT NULL UNIQUE,      -- 去重鍵
    title        TEXT NOT NULL,
    body         TEXT NOT NULL,
    published_at TIMESTAMPTZ NOT NULL,
    symbols      TEXT[] NOT NULL DEFAULT '{}',
    fetched_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- LLM 產出落地（Phase 4 前端讀這裡；也是 eval 素材庫）
CREATE TABLE summaries (
    id         BIGSERIAL PRIMARY KEY,
    trade_date DATE NOT NULL,
    provider   TEXT NOT NULL,
    model      TEXT NOT NULL,
    content    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (trade_date, provider, model)
);

-- eval 結果（Phase 4 品質頁讀這裡）
CREATE TABLE eval_runs (
    id         BIGSERIAL PRIMARY KEY,
    provider   TEXT NOT NULL,
    model      TEXT NOT NULL,
    total      INT NOT NULL,
    passed     INT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE eval_case_results (
    id        BIGSERIAL PRIMARY KEY,
    run_id    BIGINT NOT NULL REFERENCES eval_runs(id) ON DELETE CASCADE,
    case_name TEXT NOT NULL,
    passed    BOOLEAN NOT NULL,
    detail    TEXT NOT NULL DEFAULT ''
);

-- +goose Down
DROP TABLE eval_case_results;
DROP TABLE eval_runs;
DROP TABLE summaries;
DROP TABLE news;
DROP TABLE stock_daily;
DROP TABLE institutional_daily;
DROP TABLE market_index_daily;
DROP TABLE raw_payloads;
