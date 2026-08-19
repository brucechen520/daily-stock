# 架構

> daily-stock 的系統架構、模組邊界與設計取捨。
> 規劃文件見 [`docs/ROADMAP.md`](docs/ROADMAP.md)，各階段規格見 `docs/phase-*.md`。

## 一句話

每個交易日盤後，自動抓取台股公開資料，用 LLM 產出白話解讀與術語教學，推播到 Discord。**核心主張不是「用 AI 產生內容」，而是「把 LLM 當不完全可信的元件來管理」**——數字錯誤被結構性消滅，而不是靠提示詞拜託模型不要寫錯。

---

## 1. 分層與相依方向

```
cmd/daily-stock              唯一組裝點：所有相依在這裡注入
  ├─ config                  .env + 環境變數
  ├─ store                   pgx pool + goose migrations + repository
  ├─ infra                   rediskit → 分散式鎖（scheduler）+ 出站限速（market）
  ├─ ingest                  【抓取端編排】market/news 抓 → store 存
  │    ├─ market             TWSE 爬蟲（OpenAPI + RWD）
  │    └─ news               RSS 抓取 + 清洗
  ├─ scheduler               cron + 鎖 + 重試（job 以函式注入）
  ├─ digest                  【輸出端編排】讀 pg → 算統計 → 組 Section
  │    ├─ indicator          描述性統計（漲跌幅、量能、均線…）
  │    ├─ report             大盤白話摘要（唯一碰 LLM 的地方）
  │    │    ├─ llm           Provider 介面 + 三個 adapter
  │    │    └─ glossary      術語辭典（YAML 內嵌）
  │    └─ notify             Section → 通道限制 → 送出
  └─ eval                    LLM 輸出忠實度 golden set
```

### 兩個刻意的隔離

**`ingest` 與 `digest` 對稱。** 一個是抓取端的編排、一個是輸出端的編排，兩者都只做流程串接，不放領域邏輯。新增資料來源改 `ingest`，新增輸出形式改 `digest`，互不干擾。

**`market` 與 `store` 互不 import。** 兩邊各有自己的 `StockDaily` 型別，`ingest` 是唯一轉接點。代價是三個轉換函式，換來的是爬蟲改欄位不會震到資料層。

**`scheduler` 不認識業務。** 它收三個函式（`IngestMarket`、`DailyDigest`、`IngestNews`），只管 cron 觸發、鎖與重試。所以測排程不需要資料庫。

**`notify` 只管通道。** `render`（產內容）與 `sender`（塞進通道限制）分兩層——改成網頁只動 render，換成 email 只動 sender。

---

## 2. 資料流

### 抓取（每交易日 17:30，自動）

```
TWSE OpenAPI / RSS
  → Fetcher（rediskit 限速，3 req/s）
  → raw_payloads（原始 JSONB，唯一鍵 source+endpoint+trade_date）
  → 解析
  → normalized 層（market_index_daily / institutional_daily / stock_daily / news）
```

**雙層設計的用途**：解析邏輯改了可以 `daily-stock reparse` 從 `raw_payloads` 重放，不必重爬。爬蟲被限速或來源改格式時，這一層是保險。

### 輸出（每交易日 18:30，自動）

```
讀 pg 當日資料
  → report.Snapshot（連「買超/賣超」「上漲/下跌」都由程式判斷）
  → LLM（只准輸出 {欄位} 佔位符，一個數字都不准寫）
  → Validate（佔位符白名單 + 佔位符外禁止數字）→ 違規帶原因回饋重生成
  → Render（程式代入真值）
  → + 術語教室 + 免責（deterministic 附加，不經 LLM）
  → summaries 落地
  → digest 組 Section（大盤 / 自選股 / 免責）
  → notify 切成 ≤2000 字元的訊息 → Discord
```

---

## 3. 三個貫穿全案的設計

### 3.1 防幻覺是四層，不是一層

| 層 | 做什麼 |
| --- | --- |
| **1. 方向詞也不讓 LLM 說** | `Snapshot.Fields()` 產生的是「買超 517.4 億元」這種**已成句的字串**，模型拿到的不是原始數值 |
| **2. 提示詞硬禁** | 「輸出中不可出現任何阿拉伯數字」 |
| **3. 程式驗證** | 佔位符必須在白名單內；把佔位符挖掉後全文掃數字（含全形 `０-９`）；不得聲稱資料缺漏 |
| **4. 違規回饋重試** | 把驗證的具體錯誤訊息塞回提示詞重新生成，再違規才報錯 |

再加一條：**術語教室與免責聲明由程式附加**，完全不經 LLM，所以定義不可能被改寫。

這樣「數字錯」被結構性消滅，eval 只需聚焦剩下兩類：**方向曲解**（買超被講成賣壓）與**紀律違規**（出現建議性字眼）。

### 3.2 冪等三種寫法

```sql
raw_payloads   ON CONFLICT (source,endpoint,trade_date) DO UPDATE   -- 覆寫
stock_daily    ON CONFLICT (symbol,trade_date) DO UPDATE            -- 覆寫
news           ON CONFLICT (url) DO NOTHING                         -- 保留原有
```

`InsertNews` 回傳 `bool` 讓呼叫端知道「這則是不是新的」，所以 log 能印「抓 40 則、新增 3 則」。

同日重跑任意次結果相同——排程重試、手動補抓都安全。

### 3.3 優雅降級：保險壞掉不該讓主線停擺

| 情況 | 行為 |
| --- | --- |
| Redis 未設定 | 鎖與限速整組 no-op，單機開發不必先起 Redis |
| 取鎖失敗 | 無鎖執行 + 警告 |
| 限速器異常 | 本次不限速 + 警告（被 ban 的風險自負，log 看得到）|
| 鎖**被別人持有** | 跳過不執行——這不是故障，是設計意圖（多實例防重跑）|

### 3.4 不靜默

推播的失敗模式全部有出口：非交易日或資料未到 → 推簡短通知；抓取或生成失敗 → 推失敗訊息；送到一半失敗 → 不重送已送出的（避免洗頻道），改補一則「推播不完整」。

**理由**：自用工具最糟的失敗是靜默——沒收到通知時分不清「今天沒事」與「排程死了三天」。

---

## 4. 資料模型

| 表 | 用途 | 冪等鍵 |
| --- | --- | --- |
| `raw_payloads` | 原始 API 回應（審計 + 重解析）| `(source, endpoint, trade_date)` |
| `market_index_daily` | 大盤指數／成交量值 | `trade_date` |
| `institutional_daily` | 三大法人買賣超（全市場總額）| `(trade_date, actor)` |
| `stock_daily` | 個股日K（含 `adj_close` 還原價）| `(symbol, trade_date)` |
| `watchlist` | 自選股與持倉（**敏感，不進版控**）| `symbol` |
| `institutional_stock_daily` | 個股三大法人 | `(symbol, trade_date)` |
| `corporate_actions` | 除權息與還原因子 | `(symbol, ex_date)` |
| `news` | 財金新聞 | `url` |
| `summaries` | LLM 摘要產出 | `(trade_date, provider, model)` |
| `eval_runs` / `eval_case_results` | eval 執行紀錄與逐案結果 | — |

**注意**：`stock_daily` 的 OHLC 是 nullable——TWSE 對當天無成交的股票回 `"--"`，存成 NULL 而非 0。

`CREATE EXTENSION vector` 在第一份 migration 就開了，Phase 3 加向量欄位零遷移。

---

## 5. 技術選型與取捨

| 選擇 | 不選 | 理由 |
| --- | --- | --- |
| Postgres 為唯一真相來源 | 檔案 / 多個儲存 | TWSE OpenAPI 多數端點只回最新交易日，歷史是自己日積月累存出來的 |
| raw + normalized 雙層 | 只存解析結果 | 解析 bug 修完可重放，不必重爬（爬蟲有被 ban 的風險）|
| `robfig/cron` in-process | 外部排程系統 | 單一 binary 就含排程，部署物少一個元件 |
| rediskit 分散式鎖 | 無鎖 | 單機是免費保險，多實例部署時直接生效 |
| `llm.Provider` 介面 | 綁定單一供應商 | 換模型要能用同一組 eval 比較品質 |
| 模板代入 | 提示詞工程 | 「不能寫錯」比「盡量不要寫錯」可靠 |
| 個股敘述純模板 | 每檔都過 LLM | 規則化的內容交給 LLM 只是花錢買幻覺風險 |
| 上限以字元計 | 以 byte 計 | 中文一字 3 bytes，byte 計會把一則裝得下的內容多切三倍 |
| 因子表 + 物化 `adj_close` | 查詢時即時計算 | 需要索引與排序；代價是失效必須在同交易內重算 |

---

## 6. 完成度

| 模組 | 狀態 |
| --- | --- |
| `store` / `market` / `news` / `ingest` / `scheduler` | ✅ 完成 |
| `report` / `llm` / `glossary` / `eval` | ✅ 完成 |
| `indicator` | 🟡 當日漲跌幅完成；MA／量能比／距高低點／法人連買連賣規劃中 |
| `notify` / `digest` | ✅ 完成（Discord webhook）|
| 個股法人（`institutional_stock_daily`）| ⬜ schema 已建但尚無讀寫，T86 fetcher 未開工（Phase 1.5 週 2）|
| 除權息還原（`corporate_actions` → `adj_close`）| ⬜ schema 已建但尚無讀寫，TWT49U fetcher 與失效重算未開工（Phase 1.5 週 3）|
| `internal/api` + React 前端 | ⬜ 規劃中 |
| 新聞 RAG + 問答 agent | ⬜ 規劃中 |
| 因子打分與 walk-forward 回測 | ⬜ 暫緩（見 [`docs/BACKLOG.md`](docs/BACKLOG.md)）|

**紀律**：訊號需通過 walk-forward 回測才會進入任何輸出。目前所有輸出都是**描述性統計**——把公開數字換算成人看得懂的形式，不是訊號。

---

## 7. 測試策略

| 對象 | 方式 |
| --- | --- |
| `market` | `httptest` + 錄下來的真實回應（含民國年日期、非交易日空回應）|
| `report` | 模板代入正確性、非法佔位符被拒、全形數字被擋、違規回饋重試 |
| `indicator` | 純函式：NULL 收盤價、只有一根K、正負漲跌 |
| `notify` | 字元邊界切割（剛好上限／超過一個字）、rune 邊界不切壞中文 |
| `digest` | 排序、極簡版門檻、**持倉成本不得出現在 LLM prompt 或輸出**（直接斷言）|
| `eval` | 黑名單斷言邏輯本身 |
| `store` | testcontainers 起真 pg（pgvector image）測 upsert 冪等、migration 可重放、eval run 明細失敗整包回滾。`-short` 或 `SKIP_DOCKER_TESTS=1` 可跳過 |

---

## 8. 本機啟動

```bash
docker compose up -d                      # pg(pgvector) + redis
cp .env.example .env                      # 填 LLM_PROVIDER 與對應的 API key
go run ./cmd/daily-stock ingest           # 抓當日資料（migrations 自動跑）
go run ./cmd/daily-stock summary          # 生成白話摘要
go run ./cmd/daily-stock push --dry       # 組出推播內容（不送出）
go run ./cmd/daily-stock schedule         # 長駐排程
```

指令完整列表見 `daily-stock` 不帶參數的輸出。

---

## 9. 安全與紀律

- **持倉資料不進版控**：`data/*.local.sql` 已列入 `.gitignore`，repo 只留假資料範本
- **成本與損益不進 LLM prompt**：有測試直接斷言 prompt 內容
- **對外請求走限速**：所有 TWSE／RSS 出站呼叫過 rediskit RateLimiter，User-Agent 表明身分
- **免責常駐**：所有輸出標明僅供學習與研究，非投資建議
