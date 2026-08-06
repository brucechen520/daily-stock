# Phase 4 — Web Dashboard（Go API + React/TS 前端）+ 基本容器化

## 目標

把前三個 phase 的產出變成看得到的東西：**Go API（REST + SSE）+ React 18 + TypeScript + Vite 前端**，四個頁面。同時補上最基本的工程化：Dockerfile + docker-compose 一鍵起整套、GitHub Actions 跑測試與 eval gate。

雲端部署**本階段不做**——先本機 compose 跑通全功能，之後要上雲（Fly.io / VPS / GCP）時前端後端都已容器化，搬過去是配置問題不是改碼問題。

技術選型的理由（面試會被問「為什麼不用 X」）：

| 選擇 | 不選 | 理由 |
| --- | --- | --- |
| React + TS + Vite（純 SPA）| Next.js | 無 SEO/SSR 需求；純 SPA + Go API 展示「前後端分離的 API 設計」，Next 會把後端舞台搶走 |
| SSE | WebSocket | LLM streaming 是單向下行，SSE 天生匹配、可走一般 HTTP、斷線自動重連；WebSocket 是雙向才需要 |
| TanStack Query + useState | Redux | 這個規模的狀態 90% 是「server 資料的快取」，TanStack Query 就是幹這個的；引 Redux 是儀式感 |
| lightweight-charts（K線）+ Recharts（其他）| 自刻 / ECharts 全包 | K 線自己畫是天坑；TradingView 的 lib 是業界標準且免費 |

---

## 1. `internal/api` — HTTP API 設計

### REST 端點

```
GET  /api/summary?date=2026-08-07          當日摘要（已生成的讀 DB，含術語註解）
GET  /api/market/index?from=&to=           大盤指數序列（走勢圖用）
GET  /api/market/institutional?from=&to=   三大法人買賣超序列（柱狀圖用）
GET  /api/stocks/{symbol}/daily?from=&to=  個股日K OHLCV（K線圖用）
GET  /api/stocks/{symbol}/indicators       MA/KD/MACD/RSI 現值 + 序列
GET  /api/stocks/{symbol}/signal           訊號分數 + 分級 + 各因子貢獻 + 回測卡
GET  /api/glossary                         術語辭典（tooltip 用，一次載入前端 cache）
GET  /api/chats/{id}                       單次對話的完整記錄 + trace
GET  /api/eval/runs?limit=50               eval 執行歷史（pass rate 時序）
GET  /api/eval/runs/{id}                   單次 eval 明細（各類別分項 + 失敗案例）
GET  /healthz                              liveness（compose healthcheck 用）
```

原則：

- **API 只讀既有模組的產出，不含業務邏輯**——`internal/api` 是薄的 HTTP 皮，跟 CLI 共用同一層 service。CLI 與 Web 只是兩個 Deliver 出口。
- 錯誤格式統一 `{"error": {"code": "...", "message": "..."}}`；找不到資料回 404 不回空物件。
- 免責聲明由 API 帶出（`disclaimer` 欄位），前端不硬編碼——文案改一處生效。

### SSE 端點（streaming）

```
POST /api/summary/stream        重新生成摘要，LLM token 逐字推送
POST /api/chat                  agent 對話（body: {chat_id?, message}）
```

SSE 事件格式（`event:` + `data:` JSON），chat 的事件型別即 Phase 3 的 trace 結構：

```
event: token          data: {"text": "台"}                          ← LLM 逐字
event: tool_call      data: {"name": "get_institutional", "args": {...}}
event: tool_result    data: {"name": "...", "elapsed_ms": 120, "summary": {...}}
event: citation       data: {"news_id": 42, "title": "...", "url": "..."}
event: done           data: {"chat_id": "...", "usage": {...}}
event: error          data: {"code": "...", "message": "..."}
```

**tool_call / tool_result 事件是前端 trace 側欄的資料來源**——後端本來就落地 trace（Phase 3），這裡只是即時轉播，零額外成本。

### 開發期串接

Vite dev server proxy `/api → localhost:8080`，免 CORS 設定；production 由 Go 直接 serve 前端靜態檔（見 §3），同源、也免 CORS。

---

## 2. `web/` — 前端四頁面規格

```
web/
├── src/
│   ├── pages/
│   │   ├── SummaryPage.tsx     ① 今日摘要
│   │   ├── StockPage.tsx       ② 個股頁（/stocks/2330）
│   │   ├── ChatPage.tsx        ③ Agent 對話
│   │   └── EvalPage.tsx        ④ Eval 儀表板
│   ├── components/             共用：圖表包裝、術語 Tooltip、免責 Footer、SSE hook
│   ├── api/                    typed client（手寫 fetch 包裝 + 型別，端點少不用 codegen）
│   └── hooks/useSSE.ts         fetch + ReadableStream 解析 SSE（POST 端點 EventSource 不支援）
```

### ① 今日摘要頁（`/`）

- 大盤走勢圖（近 60 日收盤線圖）+ 三大法人買賣超柱狀圖（近 20 日，三色）
- LLM 白話摘要卡：預設顯示已生成內容；「重新生成」按鈕走 SSE **逐字打出**
- 術語底線虛線 + hover tooltip（glossary 對照），教學感的核心互動
- 資料日期選擇器（回看歷史摘要）

### ② 個股頁（`/stocks/:symbol`）

- K 線圖 + MA5/20/60 疊線（lightweight-charts），區間切換 3M/6M/1Y
- 籌碼因子時序：外資連買天數、買賣超 N 日均
- **訊號分數卡（本頁核心）**：分數儀表 + 分級標籤 + **各因子貢獻水平條**——「為什麼是這個分數」一眼看懂，explainability 的視覺化
- 回測卡：walk-forward 各視窗勝率、vs 大盤 baseline、最大回撤 + 「歷史回測不保證未來」聲明（每次顯示，不只 README）

### ③ Agent 對話頁（`/chat`）

- 左：訊息串。assistant 回覆逐字 streaming；訊息內引用標 `[1]`，點開新聞出處（title + url + 日期）
- 右：**tool-call trace 側欄**——每次工具呼叫即時出現一列（工具名、參數、耗時、狀態），可展開看回傳 JSON。生成中有進行狀態
- 這頁是全案差異化：市面 agent demo 都把過程藏起來，這裡把「agent 怎麼想」透明化
- 輸入框下方常駐免責：本工具不提供投資建議

### ④ Eval 儀表板（`/eval`）

- Pass rate 折線（x=eval run 時間，每次改 prompt/換模型一個點），hover 顯示當次 git commit / provider
- 分類別柱狀：數字忠實 / 引用完整 / 紀律邊界 / 工具選擇 各自 pass rate
- 失敗案例列表：輸入 → 期望 → 實際輸出，並排 diff
- 資料來源：eval run 結果落 DB（`eval_runs` / `eval_case_results` 兩張表，CI 跑完寫入）

---

## 3. 容器化（本階段只做基本版）

### Dockerfile（multi-stage：前端 build → Go build → 極小 runtime）

```dockerfile
# --- 前端 ---
FROM node:22-alpine AS web
WORKDIR /app/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build                      # → /app/web/dist

# --- 後端 ---
FROM golang:1.26-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /app/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -o /daily-stock ./cmd/daily-stock
# web/dist 用 go:embed 進 binary → 單一執行檔含前端

# --- runtime ---
FROM gcr.io/distroless/static-debian12
COPY --from=build /daily-stock /daily-stock
EXPOSE 8080
ENTRYPOINT ["/daily-stock", "serve"]
```

前端靜態檔用 `go:embed` 塞進 binary：部署物只有一個檔 + 一個 DB，之後搬任何雲都最省事。

### docker-compose.yml

```yaml
services:
  app:
    build: .
    ports: ["127.0.0.1:8080:8080"]
    env_file: .env                     # ANTHROPIC_API_KEY / VOYAGE_API_KEY…，不進 image
    environment:
      DATABASE_URL: postgres://stock:devpass_change_me@db:5432/stock?sslmode=disable
    depends_on:
      db: { condition: service_healthy }

  db:
    image: pgvector/pgvector:pg17
    environment:
      POSTGRES_USER: stock
      POSTGRES_PASSWORD: devpass_change_me   # 換成自己的
      POSTGRES_DB: stock
    ports: ["127.0.0.1:5432:5432"]           # 只綁本機（同 redis-kit 的教訓）
    volumes: [pgdata:/var/lib/postgresql/data]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U stock"]
      interval: 5s
      retries: 10

  # 地端模型（可選）：docker compose --profile local-llm up
  ollama:
    image: ollama/ollama
    profiles: ["local-llm"]
    ports: ["127.0.0.1:11434:11434"]
    volumes: [ollama:/root/.ollama]

volumes:
  pgdata:
  ollama:
```

- 排程（每日抓資料 + 生成摘要）：本階段用 host cron 打 `docker compose exec app /daily-stock ingest`，不引入額外排程元件。
- **雲端部署：延後**。到時的選項（Fly.io / GCP Cloud Run / VPS）都吃這個 image，屆時只補 secrets 管理與 managed Postgres 遷移，程式不動。

### CI（GitHub Actions，最小集）

```
on: pull_request
jobs:
  backend:  go vet + go test ./...
  frontend: npm ci + tsc --noEmit + npm run build
  eval-gate:                      # 有 API key secret 才跑（fork PR 自動略過）
    daily-stock eval --provider anthropic
    → pass rate < 門檻（例 90%）→ job fail 擋 merge
    → 結果寫回 eval_runs（給儀表板）
```

---

## 測試策略

- `internal/api`：`httptest` 打端點，斷言 JSON shape 與錯誤格式；SSE 用假 provider 餵固定 token 流，斷言事件序列（token* → done）與 `text/event-stream` header
- `web/`：`tsc --noEmit` 當型別測試 + Vitest 測 SSE 解析 hook（餵預錄 byte stream）；**不追求前端高覆蓋**，重點在 hook 與 api client
- compose 冒煙：`docker compose up -d && curl localhost:8080/healthz`（CI optional job）

## Phase 4 完成的定義（Definition of Done）

- [ ] `daily-stock serve` 起 API + 前端（單 binary，`go:embed`）
- [ ] 四頁面完成：摘要（含 SSE 逐字）、個股（K線 + 因子貢獻 + 回測卡）、對話（含 trace 側欄 + 引用）、eval 儀表板
- [ ] SSE chat 端到端：tool_call/tool_result 事件即時出現在 trace 側欄
- [ ] 每頁含免責聲明，文案由 API 下發
- [ ] `docker compose up` 一鍵起 app + pgvector，healthcheck 通過
- [ ] GitHub Actions：backend + frontend 必跑，eval gate 在有 key 時擋 merge
- [ ] eval run 結果落 DB，儀表板讀得到歷史 pass rate
- [ ] README 補：本機啟動步驟 + 架構圖 + 各頁截圖
