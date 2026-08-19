# Phase 4 切片 — 摘要頁（React + TS + Redux + Docker），一週

> **這份文件是 [phase-4.md](phase-4.md) 的最小垂直切片，不是取代它。**
> 四頁只做第一頁、API 只做這頁要的端點，但**技術深度不打折**——SSE streaming、型別安全、容器化、CI 全部做到位。
>
> **為什麼提前**：求職需要一份能展示 React + TypeScript 的實際作品，而 Phase 4 本來就規劃了 React/TS 前端。與其為了面試另外做一個 side project，不如把既有 roadmap 的東西往前挪——同一份工作，兩個用途。
>
> **代價**：Phase 1.5 週 2（T86 個股法人、MA/量能比、新聞比對）與週 3（除權息還原、報酬率）暫停，做完這個切片再回去。Discord 推播已經在跑，暫停期間不影響每日使用。

---

## 1. 目標與非目標

### 目標

一頁能動、能 demo、能被追問細節的東西：

**`/` 今日摘要頁** — 大盤走勢圖 + 三大法人柱狀圖 + LLM 白話摘要（**SSE 逐字生成**）+ 術語 hover 教學 + 日期回看。

### 非目標（這個切片明確不做）

- ❌ 個股頁、對話頁、eval 儀表板（Phase 4 完整版才做）
- ❌ K 線圖（lightweight-charts 有學習成本，摘要頁用不到）
- ❌ 雲端部署（compose 跑通即可，同 phase-4.md）
- ❌ 前端高測試覆蓋率（只測 SSE 解析 hook，那是唯一有邏輯的地方）

---

## 2. 技術選型（含與 phase-4.md 的差異）

| 選擇 | phase-4.md 原本 | 這個切片 | 理由 |
| --- | --- | --- | --- |
| 狀態/資料層 | TanStack Query | **Redux Toolkit + RTK Query** | 見下方說明 |
| UI 元件庫 | 未指定 | **Ant Design 5** | 一天內做出不醜的版面；表單、日期選擇器、Skeleton 直接有 |
| 圖表 | Recharts | **Recharts** | 不變。這頁只有折線 + 柱狀，Recharts 夠且 API 單純 |
| 建置 | Vite | **Vite + React 18 + TS strict** | 不變 |
| 路由 | 未指定 | **不用 router** | 只有一頁。之後加頁再引 React Router，現在引是空轉 |

### 為什麼從 TanStack Query 改成 RTK Query

phase-4.md 原本的判斷是：「這個規模的狀態 90% 是 server 資料的快取，TanStack Query 就是幹這個的；引 Redux 是儀式感。」**那個判斷在技術上仍然成立**，但這個切片改用 RTK Query，理由有兩層：

1. **RTK Query 不是 Redux 的舊寫法。** 它跟 TanStack Query 是同一類東西——宣告式的 server-cache 層，自動處理 loading/error/失效/重取。用它不等於回到手寫 action creator 與 reducer 樣板。所以「引 Redux 是儀式感」這句批評，針對的是傳統 Redux 寫法，不適用於 RTK Query。
2. **求職上這個技能有市場需求**，而技術上它不是壞選擇——這兩件事同時成立時，選有需求的那個。

**面試被問到時的誠實答案**：「我原本評估的是 TanStack Query，理由寫在 phase-4.md 的選型表裡。後來改用 RTK Query，因為兩者在 server-cache 這件事上的能力接近，而 RTK Query 帶來的 store 在之後要做 agent 對話頁的暫存訊息時會用到。我不會為了用 Redux 而把 server 資料手動塞進 store——那才是儀式感。」

這個回答比「我用 Redux 因為大家都用」強得多，而且是真的。

---

## 3. 後端：`internal/api`

### 端點（只做這頁要的）

```
GET  /api/summary?date=2026-08-11        當日摘要（讀 summaries 表）
GET  /api/market/index?from=&to=         大盤指數序列（走勢圖）
GET  /api/market/institutional?from=&to= 三大法人買賣超序列（柱狀圖）
GET  /api/glossary                       術語辭典（前端一次載入後 cache）
GET  /api/meta                           免責文案 + 資料截止日（文案不硬編在前端）
POST /api/summary/stream                 重新生成摘要，LLM token 逐字推送（SSE）
GET  /healthz                            compose healthcheck
```

**stretch（有時間才做）**：`GET /api/watchlist/daily?date=` — 自選股當日漲跌，直接複用 `digest.Holding`。做了這頁會更飽滿，但成本與報酬率不進 API（§5.3 的紅線同樣適用於 HTTP 出口）。

### 設計原則（沿用 phase-4.md §1）

- `internal/api` 是**薄的 HTTP 皮**，不含業務邏輯——與 CLI 共用 `digest` / `store`，CLI 和 Web 只是兩個 Deliver 出口。
- 路由用 **Go 1.22+ `net/http` 的 pattern routing**，不引 chi / gin。端點只有 7 個，標準庫夠用，也少一個要解釋的依賴。
- 錯誤格式統一 `{"error": {"code": "...", "message": "..."}}`，查無資料回 404 不回空物件。
- 免責文案由 `/api/meta` 下發，前端不硬編碼。

### SSE：這頁的技術亮點

```
event: token   data: {"text": "加"}
event: token   data: {"text": "權"}
event: done    data: {"date": "2026-08-11", "provider": "gemini", "model": "..."}
event: error   data: {"code": "provider_error", "message": "..."}
```

**這需要新增 provider 的串流能力**，目前 `llm.Provider` 只有單發的 `Generate`：

```go
// 新增，與既有 Generate 並存（既有呼叫端零改動）
type Streamer interface {
    GenerateStream(ctx context.Context, req GenerateRequest) (<-chan StreamChunk, error)
}
type StreamChunk struct {
    Text string
    Err  error
}
```

**範圍控制**：只實作 **OpenAI-compat adapter**（Gemini / Ollama 共用同一支）的串流。Anthropic adapter 不實作，`serve` 啟動時偵測 provider 是否實作 `Streamer`，沒有就退回「一次生成完再一口氣送一個 token 事件」。介面在、降級路徑清楚，之後補 Anthropic 是加法不是改法。

**防幻覺機制與串流的衝突要處理**：`report.Validate` 是對**完整輸出**做檢查（佔位符合法性、佔位符外不得有數字），串流時無法逐 token 驗證。做法是：

1. 串流過程中推送的是**未代入的原文**（含 `{index_close}` 佔位符）→ 前端即時代入真值顯示
2. 串流結束後在後端跑完整 `Validate`，失敗則發 `event: error` 並要求前端顯示「本次生成未通過驗證」

**這是這個切片最值得在面試講的設計**：串流體驗與輸出驗證是有張力的，而處理方式是「即時顯示、事後驗證、驗證失敗要讓使用者知道」，不是為了 UX 把防線拿掉。

---

## 4. 前端：`web/`

```
web/
├── src/
│   ├── App.tsx
│   ├── main.tsx
│   ├── store.ts                    Redux store 組裝
│   ├── api/
│   │   ├── client.ts               RTK Query createApi（baseQuery + endpoints）
│   │   └── types.ts                與 Go 回傳對齊的型別（手寫，端點少不用 codegen）
│   ├── features/summary/
│   │   ├── SummaryPage.tsx
│   │   ├── SummaryCard.tsx         摘要卡 + 重新生成按鈕
│   │   ├── IndexChart.tsx          大盤走勢（Recharts LineChart）
│   │   └── InstitutionalChart.tsx  三大法人（Recharts BarChart，三色）
│   ├── components/
│   │   ├── GlossaryText.tsx        掃描文字比對術語 → 包 Tooltip
│   │   └── DisclaimerFooter.tsx    文案來自 /api/meta
│   ├── hooks/useSSE.ts             fetch + ReadableStream 解析 SSE
│   └── streamSlice.ts              串流中的文字（RTK createSlice，非 server 資料）
├── tsconfig.json                   strict: true, noUncheckedIndexedAccess: true
└── vite.config.ts                  proxy /api → localhost:8080
```

### 頁面內容

- **大盤走勢圖**：近 60 個交易日收盤折線
- **三大法人柱狀圖**：近 20 日，外資／投信／自營商三色分組
- **摘要卡**：預設顯示 DB 已生成內容；按「重新生成」走 SSE 逐字打出
- **術語 tooltip**：文字中的術語加虛線底線，hover 顯示辭典定義（教學感的核心互動）
- **日期選擇器**：AntD DatePicker，回看歷史摘要；無資料的日期給明確空狀態，不是空白頁
- **免責 footer**：文案由 API 下發

### 型別紀律（TS 的展示重點）

- `tsconfig` 開 `strict` + `noUncheckedIndexedAccess`
- **API 回傳型別集中在 `api/types.ts`**，與 Go struct 的 JSON tag 一一對應；欄位可為 null 的（例如 `stock_daily` 的 OHLC）在 TS 端就是 `number | null`，強迫呼叫端處理
- 不用 `any`。真的需要逃生口時用 `unknown` + 型別守衛

### `useSSE` — 唯一需要單元測試的前端邏輯

`EventSource` 不支援 POST，所以要自己用 `fetch` + `ReadableStream` 解析。這個 hook 要處理：跨 chunk 的半行、`event:` 與 `data:` 的配對、`[DONE]`、中途 abort。用 Vitest 餵預錄的 byte stream 測，含**故意把事件切在半行的 chunk 邊界**——那是這類解析最常見的 bug。

---

## 5. 容器化與 CI

沿用 [phase-4.md §3](phase-4.md) 的 multi-stage Dockerfile（前端 build → Go build → distroless），前端 `dist` 用 `go:embed` 塞進 binary，部署物只有一個執行檔。

compose 加 `app` service（`serve` 指令），與既有 pg/redis 並存。

CI（GitHub Actions）最小集：

```
backend:  go vet + go test ./...
frontend: npm ci + tsc --noEmit + npm run build + vitest run
```

eval gate 留給 Phase 4 完整版。

---

## 6. 一週排程（全職投入，約 40–45h）

| 天 | 內容 | 時數 |
| --- | --- | --- |
| **D1** | `internal/api` 骨架 + 5 個 REST 端點 + `httptest` 測試 + `serve` 指令 | 8h |
| **D2** | `llm.Streamer` 介面 + OpenAI-compat 串流實作 + SSE 端點 + 事件序列測試 | 8h |
| **D3** | Vite + React + TS + AntD 版面骨架；RTK Query 串通 REST；兩張圖表出來 | 8h |
| **D4** | `useSSE` hook + Vitest 測試 + 摘要卡逐字生成 + 串流後驗證失敗的顯示 | 8h |
| **D5** | 術語 tooltip、日期選擇器、空狀態、免責 footer、RWD 檢查 | 6h |
| **D6** | Dockerfile + `go:embed` + compose app service + GitHub Actions | 5h |
| **D7** | README（架構圖 + 截圖 + 啟動步驟）、`/code-review` 一輪、修 review findings | 5h |

**先砍的順序**（時間不夠時）：CI → 日期選擇器 → 三大法人圖 → 術語 tooltip。
**絕對不砍**：SSE 串流、TS strict、Docker。這三個是這份作品的差異化來源。

---

## 7. Definition of Done

- [ ] `docker compose up -d` 一鍵起 pg + redis + app，`/healthz` 通
- [ ] 瀏覽器打開看得到：走勢圖、法人柱狀圖、摘要、術語 tooltip、日期回看
- [ ] 「重新生成」按鈕逐字打出摘要，且串流結束後有跑完整 `Validate`
- [ ] `tsc --noEmit` 零錯誤，全專案無 `any`
- [ ] `useSSE` 有測試，含 chunk 切在半行的情境
- [ ] `internal/api` 有 `httptest` 測試，含 SSE 事件序列與錯誤格式
- [ ] GitHub Actions backend + frontend 皆綠
- [ ] README 有架構圖與截圖，陌生人照著能在本機跑起來

---

## 8. 與求職的對應（誠實版）

這個切片能覆蓋的與不能覆蓋的：

| JD 要求 | 這個切片 |
| --- | --- |
| React | ✅ 實際寫的 SPA，含圖表、串流、tooltip |
| TypeScript | ✅ strict 模式，型別與後端對齊 |
| Redux | ✅ RTK Query + 一個 slice，且能講出選型理由 |
| Ant Design | ✅ 版面與元件 |
| Postgres | ✅ 既有的雙層資料建模、goose migrations、upsert 冪等 |
| Docker | ✅ multi-stage + compose |
| **Python 後端** | ❌ **這個專案是 Go**。面試時用過去的 Python 專案回答，不要假裝這個是 |
| 影音編碼 / RTMP / HLS | ❌ 沒有（JD 標 `is a plus`）。但 SSE streaming 至少證明對串流協定與即時資料流不陌生 |

**不要誇大**。這份作品能證明的是「React + TS + Redux 寫得出來」與「工程紀律」，不是「我有三年 React 經驗」。面試時把它定位成「近期為了補齊前端而做的實作」，誠實反而加分——對方會看你**怎麼學**，而不只是看你會什麼。
