# daily-stock — 全 Phase Roadmap

AI 台股盤後分析工具。輸入台股資料（大盤、三大法人籌碼、個股基本面/技術面）+ 財金新聞，產出：**白話解讀 + 術語教學 + 可回測的訊號研究**。

定位：**理解 + 研究**。訊號 = 「可回測的規則引擎 + 真實因子打分」產生候選，LLM 只負責解讀「為什麼」與教學，**不憑空喊單**。本專案僅供個人學習與回測研究使用，不對外提供投資建議或投資顧問服務。

> ⚠️ **範圍與定位聲明**：本工具產出之訊號、解讀與任何內容均為研究與教育性質，非投資建議，不構成任何買賣邀約。開發者不對使用本工具所生之投資決策負責。
>
> 📋 產品需求規格見 [phase-0-prd.md](phase-0-prd.md)（產品化情境的規格演練，現階段未對外；若未來對外需先完成其 §1.1 法遵行動項）。本 ROADMAP 是它的工程對應。

## 設計原則

1. **數字防幻覺（輸入端）**：股價 / EPS / 買賣超一律從 API 取真值，餵給 LLM。LLM 只解讀，不生數字。
2. **輸出忠實度驗證（輸出端）**：建立小型 eval set，定期檢查 LLM 解讀是否忠於原始數據，避免曲解或誇大。
3. **LLM provider 可切換**：抽 `llm.Provider` 介面，Claude / Gemini / 地端（Ollama）config 一行切，上層 agent 零改動。
4. **訊號可回測**：訊號來自規則 + 因子分數，規則要能對歷史資料回測（並避開 look-ahead bias / overfitting），否則不出訊號。
5. **先計劃再動手**：每 phase 有獨立 plan doc，看過才實作。

## 架構分層

```
① Ingestion   抓行情/籌碼/財報/新聞 → 落地
② Compute     算技術指標、籌碼因子（連買連賣天數）、基本面因子
③ Signal      因子打分 → 分級 → 候選訊號（+ 回測驗證，含 walk-forward）
④ LLM         provider 介面 → 白話解讀 + 術語註解 + 訊號說明 + 輸出忠實度 eval
⑤ Deliver     CLI 盤後摘要 → 每日晨報 → web dashboard
```

## 模組

| package | 職責 |
| --- | --- |
| `internal/store` | pgx 連線池 + goose migrations + repository（upsert 冪等）|
| `internal/scheduler` | cron 排程 + rediskit lock + 重試（全自動抓取）|
| `internal/llm` | Provider 介面 + Anthropic / OpenAI-compat adapter + factory |
| `internal/market` | TWSE 爬蟲 Fetcher + mock（查詢一律走 store）|
| `internal/indicator` | 技術指標（MA/KD/MACD/RSI）+ 籌碼因子（Phase 2）|
| `internal/signal` | 因子打分 → 訊號 + 回測（walk-forward，Phase 2）|
| `internal/eval` | LLM 輸出忠實度 eval harness（Phase 1 起）|
| `internal/glossary` | 投資術語辭典 + 自動註解 |
| `internal/news` | 財金新聞抓取 + RAG（Phase 3）|
| `internal/embed` | Embedding provider 介面 + Voyage / OpenAI-compat / Ollama adapter（Phase 3）|
| `internal/agent` | tool-use agent loop + 工具註冊 + trace（Phase 3）|
| `internal/report` | 組資料 → prompt → LLM → 摘要 |
| `internal/api` | HTTP server：REST + SSE streaming（Phase 4）|
| `web/` | React + TypeScript 前端（Phase 4）|
| `cmd/daily-stock` | CLI 入口 |

## Phases

| Phase | 目標 | 產出 | 狀態 |
| --- | --- | --- | --- |
| **1** | 盤後白話摘要 MVP + 自動資料管線 | 排程器自動抓 TWSE 大盤/三大法人/個股日K + 新聞 RSS → **直接落地 Postgres(pgvector)**；LLM 白話摘要（模板代入防幻覺）+ 術語註解；provider 可切；eval set。compose 起 pg/redis（kafka/ollama 走 profile）| **本階段**，詳見 `phase-1.md` |
| **2'** | 回測極簡版（timebox 一週）| 單一因子（外資連買天數）+ 固定參數 + walk-forward 跑通一輪。目的是讓「訊號經回測」為真，不是做完整量化平台 | 詳見 `phase-2.md`（只做「極簡版」小節範圍）|
| **3** | 新聞 RAG + 個股問答 agent | 新聞 embedding(pgvector) + hybrid 檢索 + LLM tool-use agent + agent eval | **AI 練習主菜**，詳見 `phase-3.md` |
| **4** | Web dashboard + 基本容器化 | Go API（REST + SSE streaming）+ React/TS 前端四頁 + Dockerfile/compose + CI eval gate；雲端部署延後 | 詳見 `phase-4.md` |
| 2'' | 回測完整版 | 多因子、調參、交易成本模型、survivorship 處理 | 想衝量化深度時再回來（`phase-2.md` 完整範圍）|

> **順序理由**：本專案的學習目標是 AI 應用（RAG / embedding / agent / eval harness），這些全部在 Phase 3；Phase 2 是量化工程，AI 含量最低，故降為極簡 timebox 版，保住「回測站得住才出訊號」的原則即可。求職敘事要衝量化深度時再回 2''。

## 資料來源

| 類別 | 來源 | Phase |
| --- | --- | --- |
| 大盤指數 / 個股日K | TWSE OpenAPI（免費、盤後）| 1 |
| 三大法人買賣超（外資/投信/自營商）| TWSE OpenAPI | 1 |
| 融資融券 / 借券 / 財報 | FinMind | 2 |
| 財金新聞 | 鉅亨 / 經濟日報 RSS | 1（抓取落地）/ 3（RAG）|
| 即時報價 | 富果 Fugle / 券商 API | 4 |

## Known Limitations

- 目前訊號未考慮大盤系統性風險（例如系統性下跌時個股因子可能全數失真）。
- 回測資料範圍受限於免費 API 可取得之歷史區間，樣本外驗證力有限。
- LLM 解讀品質依賴 eval set 覆蓋率，目前 eval set 僅涵蓋常見情境，非全面驗證。
- 地端模型（Ollama）在 tool-use（Phase 3）情境下能力較弱，需搭配支援 function calling 的模型（如 Qwen、Llama 3.1+）或 fallback JSON 模式。

## 風險與紀律

- **免責**：所有輸出標「非投資建議，僅供理解與學習研究使用」，且產品定位為個人研究工具，不對外提供訊號服務。
- **版權**：新聞優先官方 RSS/API，爬蟲看 robots，不整篇轉存。
- **回測前不出訊號**：Phase 1 只做「白話摘要」，不產生任何買賣建議；訊號需 Phase 2 回測站得住（含避免 look-ahead bias）才納入輸出。
- **地端模型 tool-use 弱**：Phase 3 的 tool-use，地端選支援 function calling 的模型（Qwen / Llama 3.1+）或 fallback JSON 模式。
