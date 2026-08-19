# daily-stock

AI 台股盤後分析研究工具。輸入台股公開資料（大盤、三大法人籌碼、個股日K）+ 財金新聞，產出**白話解讀 + 術語教學 + 可回測的因子訊號研究**，並附完整的 LLM 輸出驗證（eval harness）與問答 agent。

定位：**理解 + 研究**。訊號來自「可回測的規則引擎 + 真實因子打分」，LLM 只負責解讀「為什麼」與教學，**不憑空喊單**。

> ⚠️ **範圍與定位聲明**：本專案僅供個人學習與回測研究使用，不對外提供服務。所有產出均為研究與教育性質，非投資建議，不構成任何買賣邀約。開發者不對使用本工具所生之投資決策負責。

## 專案架構

| 面向 | 具體實物 |
| --- | --- |
| **AI 工程** | prompt 防幻覺（模板代入，數字由程式填）、LLM/Embedding 雙 provider 抽象、RAG（pgvector + hybrid 檢索）、tool-use agent（含 trace）、三層 eval harness 進 CI |
| **後端** | Go 分層架構、Postgres/pgvector 資料建模（raw + normalized 雙層）、爬蟲 + 排程自動 ingestion（冪等 upsert）、REST + SSE API、testcontainers 整合測試 |
| **全端** | React + TS 前端四頁：摘要（SSE 逐字）、個股（K線 + 因子貢獻）、agent 對話（tool-call trace 側欄）、eval 儀表板 |

## 設計原則

1. **數字防幻覺（輸入端）**：真值一律從 API 取得餵給 LLM；LLM 只解讀不生數字，數字由程式代入模板。
2. **輸出忠實度驗證（輸出端）**：eval set 檢查解讀是否忠於原始數據；改 prompt / 換 provider 必重跑，CI 當 merge gate。
3. **Provider 可切換**：`llm.Provider` 與 `embed.EmbeddingProvider` 雙介面，Claude / 地端（Ollama）config 一行切。
4. **訊號可回測**：規則要能對歷史資料 walk-forward 回測（避開 look-ahead / survivorship / overfitting），站不住就不出訊號。
5. **先計劃再動手**：每 phase 有獨立 plan doc（`docs/`），看過才實作。

## 架構分層

```
① Ingestion   排程爬蟲抓行情/籌碼/新聞 → Postgres(pgvector) 落地，全自動
② Compute     技術指標（MA/KD/MACD/RSI）、籌碼因子（連買連賣天數）
③ Signal      因子打分 → 分級 → 候選訊號（walk-forward 回測驗證）
④ LLM         provider 介面 → 白話解讀 / RAG / agent + eval harness
⑤ Deliver     CLI → REST + SSE API → React web dashboard
```

## Phases 與文件索引

| Phase | 內容 | 文件 |
| --- | --- | --- |
| **0** | 產品需求規格書（PRD）：功能模組 M1–M9、使用者故事、驗收標準、法遵紅線。**產品化情境的規格演練，現階段未對外** | [docs/phase-0-prd.md](docs/phase-0-prd.md) |
| 全局 | 工程順序、模組表、資料來源、風險紀律 | [docs/ROADMAP.md](docs/ROADMAP.md) |
| 全局 | 已決策暫緩的項目（含暫緩理由與重啟條件）| [docs/BACKLOG.md](docs/BACKLOG.md) |
| **1** | 盤後白話摘要 MVP：TWSE 資料 + LLM 摘要 + 術語 + eval set | [docs/phase-1.md](docs/phase-1.md) |
| **1.5** | **自選股每日追蹤（自用版）**：watchlist 持倉 + 個股法人 + 除權息還原 + 描述性統計 + Discord 推播。**本階段** | [docs/phase-1.5.md](docs/phase-1.5.md) |
| **2'** | 回測極簡版（timebox）：單因子 walk-forward 跑通；完整量化版留作 2''（**暫緩**）| [docs/phase-2.md](docs/phase-2.md) |
| **3** | 新聞 RAG（pgvector + hybrid 檢索）+ 個股問答 agent + agent eval | [docs/phase-3.md](docs/phase-3.md) |
| **4** | Go API（REST + SSE）+ React/TS 前端四頁 + Docker/CI | [docs/phase-4.md](docs/phase-4.md) |

> 順序理由：學習目標是 AI 應用，RAG / embedding / agent / eval 全在 Phase 3，故升為主菜；Phase 2 是量化工程（AI 含量最低），降為極簡 timebox 版，保住「回測站得住才出訊號」的原則。
>
> Phase 1.5 是後來插隊的：專案第一優先從「作品集」改為「自己每天實際使用」，最痛的需求變成「我持有的這幾檔今天怎麼了」。它仍只做**描述性統計**，不出訊號，紅線不動。決策過程見 [phase-1.5.md](docs/phase-1.5.md) §1。

## 快速開始

```bash
# 基礎設施（Phase 1 起）：pg(pgvector) + redis；kafka / ollama 走 profile
docker compose up -d
cp .env.example .env            # 填 ANTHROPIC_API_KEY（或用 --provider ollama 免金鑰）

# 自動排程（平日盤後抓資料 + 生成摘要、每 30 分鐘抓新聞）
go run ./cmd/daily-stock schedule

# 手動指令（補抓 / 回補 / debug）
go run ./cmd/daily-stock ingest --date 2026-08-07
go run ./cmd/daily-stock summary --date 2026-08-07
go run ./cmd/daily-stock eval

# Phase 4 之後（app 容器化 + 前端）
open http://localhost:8080
```

（實作進行中，指令以各 phase 文件的 DoD 為準。）

## 資料來源

| 類別 | 來源 | Phase |
| --- | --- | --- |
| 大盤指數 / 個股日K / 三大法人 | TWSE OpenAPI（免費、盤後）| 1 |
| 融資融券 / 財報 | FinMind | 2 |
| 財金新聞 | 鉅亨 / 經濟日報 RSS | 1（抓取落地）/ 3（RAG）|

## Known Limitations

- 訊號未考慮大盤系統性風險（系統性下跌時個股因子可能全數失真）。
- 回測受限免費 API 的歷史區間；股票池含 survivorship bias（無歷史成分股資料），已以「大型權值股 + 上市年限門檻」折衷並誠實揭露。
- eval set 覆蓋常見情境，非全面驗證。
- 地端模型 tool-use 較弱：需支援 function calling 的模型（Qwen / Llama 3.1+）或 fallback JSON 模式，品質作為對照組。
- 檢索未加 reranker（現階段資料量不需要）。

## 風險與紀律

- **免責**：所有輸出（含前端每頁）標「非投資建議，僅供理解與學習研究使用」；定位為個人研究工具，不對外提供訊號服務。
- **版權**：新聞優先官方 RSS/API，爬蟲遵守 robots.txt，不整篇轉存。
- **回測前不出訊號**：Phase 1 只做白話摘要；訊號需通過 walk-forward 驗證才納入輸出，且每次顯示「歷史回測不保證未來」。

## 姊妹專案

- [go-redis-kit](https://github.com/brucechen520/go-redis-kit) — Redis 觀念 + rediskit 封裝（本專案的 API cache / rate limit 直接取用）
- go-kafka-kit — Kafka 觀念 + kafkakit 封裝
