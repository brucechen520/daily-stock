-- +goose Up

-- 自選股 + 持倉。資料敏感（成本、股數），seed 檔走 data/positions.local.sql 且不進版控。
CREATE TABLE watchlist (
    symbol     TEXT PRIMARY KEY,
    shares     BIGINT NOT NULL DEFAULT 0,     -- 0 = 只觀察不持有
    avg_cost   NUMERIC(10,2),                 -- 均價；分批買進自行算好再更新（逐筆交易表見 BACKLOG）
    bought_at  DATE,                          -- 首次買進日；判定領到哪幾次配息（總報酬用）
    note       TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 個股三大法人（TWSE T86，單位：股）。Phase 1.5 週 2 開始寫入。
CREATE TABLE institutional_stock_daily (
    symbol      TEXT NOT NULL,
    trade_date  DATE NOT NULL,
    foreign_net BIGINT NOT NULL,              -- 外陸資（不含外資自營商）買賣超股數
    trust_net   BIGINT NOT NULL,              -- 投信
    dealer_net  BIGINT NOT NULL,              -- 自營商合計（自行買賣 + 避險）
    total_net   BIGINT NOT NULL,              -- 三大法人合計
    PRIMARY KEY (symbol, trade_date)
);

-- 公司行動：除權息（TWSE TWT49U）。Phase 1.5 週 3 開始寫入。
-- adj_factor = ref_price / prev_close，TWSE 的參考價已含現金股息與配股。
CREATE TABLE corporate_actions (
    symbol     TEXT NOT NULL,
    ex_date    DATE NOT NULL,                 -- 除權息交易日
    kind       TEXT NOT NULL,                 -- '息' / '權' / '權息'
    prev_close NUMERIC(10,2) NOT NULL,
    ref_price  NUMERIC(10,2) NOT NULL,
    value      NUMERIC(10,4) NOT NULL,        -- 權值+息值
    adj_factor NUMERIC(12,8) NOT NULL,
    PRIMARY KEY (symbol, ex_date)
);

-- 還原收盤價（物化 cache）。失效規則：寫入 corporate_actions 時，
-- 同一 transaction 內重算該檔全部歷史，不可交給事後補算。
ALTER TABLE stock_daily ADD COLUMN adj_close NUMERIC(10,2);

-- +goose Down
ALTER TABLE stock_daily DROP COLUMN adj_close;
DROP TABLE corporate_actions;
DROP TABLE institutional_stock_daily;
DROP TABLE watchlist;
