-- 持倉 seed 範本。複製成 data/positions.local.sql 再填自己的資料——
-- .local.sql 已列入 .gitignore，成本與股數不會進版控。
--
--   cp data/positions.example.sql data/positions.local.sql
--   # 編輯 positions.local.sql
--   docker compose exec -T db psql -U stock -d stock < data/positions.local.sql
--
-- 欄位：
--   symbol     股票代號（字串，前面的 0 要留著，例如 '0050'）
--   shares     持有股數（1 張 = 1000 股）；只觀察不持有就填 0
--   avg_cost   平均成本；只觀察填 NULL
--   bought_at  首次買進日；判定領到哪幾次配息（總報酬用），只觀察填 NULL
--   note       備註，選填

INSERT INTO watchlist (symbol, shares, avg_cost, bought_at, note) VALUES
    ('2330', 2000,  985.00, '2025-03-14', '核心持股'),
    ('2317', 3000,  198.50, '2025-06-02', ''),
    ('0050',    0,    NULL,         NULL, '只觀察，不持有')
ON CONFLICT (symbol) DO UPDATE SET
    shares    = EXCLUDED.shares,
    avg_cost  = EXCLUDED.avg_cost,
    bought_at = EXCLUDED.bought_at,
    note      = EXCLUDED.note;
