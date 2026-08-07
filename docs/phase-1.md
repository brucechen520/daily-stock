# Phase 1 — 盤後白話摘要 MVP（自動抓取 + Postgres 落地）

## 目標

**全自動資料管線 + LLM 白話摘要**：排程器每日自動抓 TWSE 大盤指數、三大法人買賣超、個股日K與財金新聞，**直接落地 PostgreSQL**（不經手動、不落散裝檔案），再由 LLM 生成白話解讀 + 術語註解。**不產生任何買賣訊號**。

同時建立三個地基，後面每個 phase 都踩在上面：

1. **Postgres 為唯一真相來源**：day 1 就用 pgvector image，Phase 3 加向量欄位零遷移；Phase 2 回測直接查歷史表。
2. **Provider 可切換**：`llm.Provider` 介面，Claude / Ollama config 一行切。
3. **輸出忠實度 eval**：把 LLM 當不完全可信元件管理的驗證機制。

## 產出範例（示意）

```
📊 2026-08-07 盤後摘要

大盤：加權指數收 22,150 點，漲 85 點（+0.39%）
→ 白話：今天大盤小漲，屬於溫和整理格局，沒有明顯方向性表態。

三大法人：外資買超 32 億元，投信賣超 5 億元
→ 白話：外資站買方，是短線偏多的訊號之一，但投信同步賣超，
   顯示法人對後市看法不完全一致。

【術語小教室】
買超/賣超：指法人當日買進金額扣掉賣出金額後的淨額。

⚠️ 以上內容僅供學習與理解市場動態使用，非投資建議。
```

---

## 1. 基礎設施：docker-compose（Phase 1 就建，infra only）

**原則：compose 只裝基礎設施，app 本體開發期在 host 用 `go run` 跑**（迭代快）；app 容器化留給 Phase 4。

```yaml
# docker-compose.yaml
services:
  db:
    image: pgvector/pgvector:pg17          # day 1 就用 pgvector image，Phase 3 免換
    environment:
      POSTGRES_USER: stock
      POSTGRES_PASSWORD: devpass_change_me # 換成你自己的
      POSTGRES_DB: stock
    ports: ["127.0.0.1:5432:5432"]         # 只綁本機
    volumes: [pgdata:/var/lib/postgresql/data]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U stock"]
      interval: 5s
      retries: 10

  redis:
    image: redis:7
    command: redis-server --requirepass devpass_change_me --maxmemory 128mb --maxmemory-policy allkeys-lru
    ports: ["127.0.0.1:6379:6379"]

  # --- 以下 profile 隔開，用到才起 ---

  kafka:                                    # profile: pipeline（見 §2 kafka 的角色）
    image: apache/kafka:3.9                 # KRaft 單 broker
    profiles: ["pipeline"]
    ports: ["127.0.0.1:9092:9092"]
    environment:
      KAFKA_NODE_ID: 1
      KAFKA_PROCESS_ROLES: broker,controller
      KAFKA_CONTROLLER_QUORUM_VOTERS: 1@kafka:29093
      KAFKA_LISTENERS: PLAINTEXT://:29092,CONTROLLER://:29093,HOST://:9092
      KAFKA_ADVERTISED_LISTENERS: PLAINTEXT://kafka:29092,HOST://localhost:9092
      KAFKA_LISTENER_SECURITY_PROTOCOL_MAP: PLAINTEXT:PLAINTEXT,CONTROLLER:PLAINTEXT,HOST:PLAINTEXT
      KAFKA_CONTROLLER_LISTENER_NAMES: CONTROLLER
      KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR: 1

  ollama:                                   # profile: local-llm，地端模型
    image: ollama/ollama
    profiles: ["local-llm"]
    ports: ["127.0.0.1:11434:11434"]
    volumes: [ollama:/root/.ollama]

volumes:
  pgdata:
  ollama:
```

```bash
docker compose up -d                        # 日常：pg + redis
docker compose --profile local-llm up -d    # 要用 Ollama 時
docker compose --profile pipeline up -d     # 要練 kafka 事件管線時
```

### 各元件在本專案的角色（誠實版）

| 元件 | 角色 | 必要性 |
| --- | --- | --- |
| **Postgres(pgvector)** | 唯一真相來源：原始 payload、正規化行情、新聞、摘要、eval 結果 | **必要** |
| **Redis** | 經 [rediskit](https://github.com/brucechen520/go-redis-kit)：對外 API 的 rate limit（防被 TWSE ban）、排程分散式鎖（多實例防重抓）、glossary/摘要 cache | 建議（吃自己狗糧） |
| **Kafka** | 經 kafkakit：ingestion 完成後發 `raw.ingested` 事件，Phase 3 的 embedding worker 當 consumer 解耦消費 | **可選**。單機批次抓取其實用不到，放進來是為了練 kafkakit + 預留 Phase 3 解耦；core path 不依賴它（feature flag 控制發不發事件） |
| **Ollama** | 地端 LLM 對照組 | 可選 |

> 誠實原則延續：不假裝 kafka 是必需品。它的教學價值 > 工程必要性，文件講明白，面試被問「為什麼用 kafka」時答案是「事件解耦的預留 + 練習」，不是硬拗。

---

## 2. 資料管線：爬蟲 → Postgres

### 2.1 資料源與抓取方式

| 資料 | 端點 | 頻率 | 注意 |
| --- | --- | --- | --- |
| 大盤成交資訊 | TWSE OpenAPI `/v1/exchangeReport/FMTQIK` | 每交易日盤後 | 回傳當月每日：指數、成交量值、漲跌 |
| 三大法人買賣金額 | TWSE OpenAPI `/v1/fund/BFI82U` | 每交易日盤後 | 外資/投信/自營商買賣超 |
| 個股日K（全市場） | TWSE OpenAPI `/v1/exchangeReport/STOCK_DAY_ALL` | 每交易日盤後 | **只回當日**，一次全市場，落地後過濾 watchlist |
| 個股歷史回補 | `www.twse.com.tw/exchangeReport/STOCK_DAY?date=&stockNo=` | backfill 一次性 | 按「股票×月」分頁抓，配 rate limit 慢慢補 |
| 財金新聞 | 鉅亨 / 經濟日報 RSS | 每 30 分鐘 | Phase 1 只**抓取落地**（存 `news` 表）；chunk/embedding/檢索是 Phase 3 |

**TWSE OpenAPI 的關鍵現實**：多數端點**只回最新交易日、沒有歷史查詢參數**。所以「每天準時抓、抓了就存」不是優化而是必需——歷史資料是自己日積月累存出來的。這正是 day 1 就上 Postgres 的理由；漏抓一天 = 那天資料要走 backfill 路徑補。

### 2.2 Schema（migrations 用 goose，SQL 檔進版控）

**兩層設計：raw 層存原始 payload（審計 + 重解析），normalized 層給查詢**：

```sql
-- 001_init.up.sql
CREATE EXTENSION IF NOT EXISTS vector;      -- Phase 3 會用，先開

-- raw 層：原始回應全存，解析邏輯改了可以重放，不用重新爬
CREATE TABLE raw_payloads (
    id         BIGSERIAL PRIMARY KEY,
    source     TEXT NOT NULL,               -- 'twse' / 'cnyes-rss' / ...
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
    amount     BIGINT NOT NULL              -- 成交金額
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
    trade_date DATE NOT NULL,
    open  NUMERIC(10,2), high NUMERIC(10,2),
    low   NUMERIC(10,2), close NUMERIC(10,2),
    volume BIGINT,
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
    symbols      TEXT[] DEFAULT '{}',
    fetched_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- LLM 產出也落地（Phase 4 前端讀這裡；也是 eval 的素材庫）
CREATE TABLE summaries (
    id         BIGSERIAL PRIMARY KEY,
    trade_date DATE NOT NULL,
    provider   TEXT NOT NULL,               -- 'anthropic' / 'ollama'
    model      TEXT NOT NULL,
    content    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (trade_date, provider, model)
);
```

**冪等鐵律**：所有寫入走 `INSERT ... ON CONFLICT ... DO UPDATE`。同一天重跑 ingest 任意次結果相同——排程重試、手動補抓都不會產生重複資料。

### 2.3 排程：`daily-stock schedule`（長駐程序，不靠手動）

```
17:30 平日   ingest market   抓大盤 + 法人 + 全市場日K → upsert pg
             （TWSE 盤後資料 ~17:00 後才完整，17:30 起跑 + 失敗重試三次、間隔 10 分鐘）
18:30 平日   generate        當日摘要生成 → summaries 表
*/30 全週    ingest news     RSS 拉新聞 → url 去重 upsert
```

- 用 `robfig/cron` in-process，不引入外部排程系統。
- **rediskit Locker 包每個 job**（`lock:ingest-market`）：未來多實例部署不重抓；單機時也是免費保險。
- 非交易日：TWSE 回空資料 → 記 log 跳過，不算失敗。
- 手動指令並存（backfill、補跑、debug 用）：

```bash
daily-stock ingest --date 2026-08-07          # 手動補某日
daily-stock backfill --symbol 2330 --from 2025-01   # 歷史回補（走 STOCK_DAY 月分頁）
daily-stock summary --date 2026-08-07          # 生成摘要（讀 pg，不現抓）
daily-stock schedule                           # 長駐排程器
```

### 2.4 對外請求紀律

- **rate limit**：rediskit `RateLimiter` 包所有 TWSE/RSS 出站呼叫（如 3 req/s），backfill 尤其要慢——被 ban 整條管線停擺。
- **cache**：同日重複查詢直接命中 pg，不重打 API（`raw_payloads` 的 UNIQUE 鍵就是判斷依據）。
- **User-Agent 表明身分**、遵守 robots.txt（新聞）、不整篇轉存展示（版權紀律見 ROADMAP）。

### 2.5 Kafka 事件（可選，feature flag）

```
ingest 完成 → kafkakit Producer 發 raw.ingested {source, trade_date, table, count}
```

Phase 1 只發不消費（觀察用）；Phase 3 的 embedding worker 訂閱 `raw.ingested`（source=news）觸發 chunk+embed，取代輪詢。`ENABLE_KAFKA=false` 時整段跳過,core path 零依賴。

---

## 3. 子模組拆解

| package | 職責 |
| --- | --- |
| `internal/store` | pgx 連線池 + goose migrations + 各表的 repository（upsert 語意） |
| `internal/market` | `MarketDataProvider` 介面 + TWSE client（真實爬取）+ MockClient（測試） |
| `internal/news` | RSS 抓取 + 清洗 + `news` 表落地（chunk/embed 留 Phase 3） |
| `internal/scheduler` | cron 註冊 + rediskit lock 包裝 + 重試 |
| `internal/llm` | `Provider` 介面 + Anthropic / Ollama adapter + factory |
| `internal/report` | 讀 pg 當日資料 → 模板 + LLM 解讀 → `summaries` 落地 |
| `internal/glossary` | 術語辭典（YAML）+ 註解 |
| `internal/eval` | 輸出忠實度 eval harness |

### `internal/market` 介面（改為讀寫分離：抓是抓、查是查）

```go
// 抓取端：爬蟲實作，回原始 + 解析後結構
type Fetcher interface {
    FetchIndexDaily(ctx context.Context) (*IndexData, json.RawMessage, error)
    FetchInstitutional(ctx context.Context) ([]InstitutionalData, json.RawMessage, error)
    FetchAllStockDaily(ctx context.Context) ([]StockDaily, json.RawMessage, error)
}
// 查詢端：report/signal/api 都從 pg 查，不碰爬蟲
// （直接用 internal/store 的 repository，不再抽一層 interface）
```

`json.RawMessage` 就是進 `raw_payloads` 的原始 payload——**解析 bug 修了之後可以 `daily-stock reparse` 重放,不用重爬**。

### `internal/llm` — Provider 介面

```go
type Provider interface {
    Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error)
}
type GenerateRequest struct {
    SystemPrompt string
    UserPrompt   string
    MaxTokens    int
}
```

（Phase 3 會擴充 `Messages` + `Tools`,先留單輪。）

### `internal/report` — 防幻覺:數字不讓 LLM 寫

**模板代入法**:LLM 產生「引用欄位的解讀文」,數字由程式代入,想寫錯都寫不了:

```
LLM 輸出:「外資買超 {foreign_net} 億元，顯示…」   ← 只准寫佔位符
渲染層:  程式把 pg 查出的真值填進 {foreign_net}    ← 數字永遠是真的
驗證:    佔位符必須 ∈ 已知欄位集,否則 reject 重生成
```

Prompt 骨架:

```
你是台股市場的說明員。以下欄位代表今日真實數據,生成解讀時**只能用 {欄位名} 佔位符引用數值,
不可自行寫出任何數字**。
可用欄位:{index_close} {index_change} {index_change_pct} {foreign_net} {trust_net} {dealer_net}
規則:1) 不可給買賣建議,只解釋現象 2) 資料缺失就寫「今日無相關資料」 3) 至少教學一個術語
```

### `internal/eval` — 輸出忠實度驗證

- 模板方案讓「數字錯」被結構性消滅,eval 聚焦剩下兩類:
  1. **方向曲解**(買超 32 億被解讀成「賣壓沉重」):LLM-as-judge + rubric,judge prompt 進版控
  2. **紀律違規**(出現建議性字眼):黑名單詞程式斷言
- Golden set 20–30 組存 repo(YAML),`daily-stock eval` 可重跑,結果落 `eval_runs` 表(Phase 4 儀表板用)
- 改 prompt / 換 provider 必重跑,記 pass rate

---

## 4. 測試策略

- `internal/store`:**testcontainers-go 起真 pg** 測 upsert 冪等(同日寫兩次結果相同)、migration 可重放
- `internal/market`:`httptest` 假 TWSE server 餵**錄下來的真實 response fixture**(含非交易日空回應、格式怪異的民國年日期——TWSE 用民國年,解析要專門測)
- `internal/news`:髒 HTML 清洗對答案;url 去重
- `internal/scheduler`:注入假 clock 測觸發時間;lock 被持有時跳過不重抓
- `internal/llm`:`httptest` 假 API server,不燒額度
- `internal/report`:模板代入正確性(佔位符全被真值替換、非法佔位符被 reject)
- `internal/eval`:黑名單斷言邏輯本身的單元測試

## 5. Phase 1 完成的定義(Definition of Done)

- [ ] `docker compose up -d` 起 pg(pgvector) + redis,healthcheck 過
- [ ] goose migrations 建齊 §2.2 全部表
- [ ] `daily-stock schedule` 長駐:平日 17:30 自動抓大盤/法人/日K、18:30 自動生成摘要、每 30 分鐘抓新聞,全程無手動
- [ ] 冪等驗證:同日重跑 ingest 任意次,各表資料不重複不變質
- [ ] `raw_payloads` 有原始 payload,`daily-stock reparse` 可重放解析
- [ ] `daily-stock backfill` 可回補個股歷史(帶 rate limit)
- [ ] 出站呼叫走 rediskit rate limit;排程 job 有 rediskit lock
- [ ] 摘要走模板代入,數字非 LLM 生成;`summaries` 落地
- [ ] `llm.Provider` Claude 與 Ollama 至少一個跑通
- [ ] 術語教學 ≥ 10 詞彙
- [ ] eval set ≥ 20 組,`daily-stock eval` 可重跑,結果落 `eval_runs`
- [ ] README 附 demo 輸出與排程 log 截圖
