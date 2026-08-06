# Phase 3 — 新聞 RAG + 個股問答 Agent

## 目標

兩個交付物，共用同一套資料底座：

1. **新聞 RAG**：財金新聞抓取 → 清洗 → chunking → embedding → pgvector 落地 → hybrid 檢索。讓「解讀」有新聞脈絡可引用（例：外資大買 → 檢索出「台積電 CoWoS 擴產」相關新聞佐證）。
2. **個股問答 Agent**：使用者問「2330 最近怎麼樣？」，LLM 透過 tool-use 自己決定調哪些工具（行情 / 籌碼 / 指標 / 訊號分數 / 新聞檢索），彙整成有引用來源的回答。

這是整個專案 AI 技術密度最高的 phase：embedding、向量檢索、hybrid search、agent loop、agent eval 全在這裡。Phase 1 的兩條紀律延續且升級：

- **數字防幻覺**：數字一律來自 tool 回傳值，LLM 只引用。
- **輸出可驗證**：回答必附引用（新聞 id / 工具名），無法溯源的句子視為缺陷。

---

## 架構總覽

```
┌── Ingestion（排程）─────────────────────────────────┐
│  RSS(鉅亨/經濟日報) → 去重 → 清洗 → chunking        │
│      → embedding → pgvector                          │
└──────────────────────────────────────────────────────┘
                          │
┌── 查詢時 ────────────────▼───────────────────────────┐
│  使用者問題                                           │
│    → Agent loop（LLM + tool 定義）                    │
│        ├─ tool: get_stock_daily / get_institutional   │
│        ├─ tool: get_indicators / get_signal_score     │
│        └─ tool: search_news（hybrid 檢索 pgvector）   │
│    → 彙整回答 + 引用 → SSE streaming 給前端/CLI       │
└──────────────────────────────────────────────────────┘
```

---

## 子模組拆解

### 1. `internal/news` — 抓取、清洗、chunking

**抓取**：官方 RSS 優先（鉅亨、經濟日報），`robots.txt` 尊重，不整篇轉存原文對外展示（版權紀律，見 ROADMAP）。落地欄位：

```sql
CREATE TABLE news (
    id           BIGSERIAL PRIMARY KEY,
    source       TEXT NOT NULL,          -- 'cnyes' / 'money-udn'
    url          TEXT NOT NULL UNIQUE,   -- 去重鍵
    title        TEXT NOT NULL,
    body         TEXT NOT NULL,          -- 清洗後純文字
    published_at TIMESTAMPTZ NOT NULL,
    symbols      TEXT[] DEFAULT '{}',    -- 內文提及的股號（正則 + 公司名對照表抽取）
    fetched_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**清洗**：去 HTML、去廣告尾綴、去「延伸閱讀」區塊。清洗規則要有單元測試（餵髒 HTML 對答案）。

**Chunking 策略**（財金新聞短，不需要複雜遞迴切分）：

- 以**段落**為單位聚合，每 chunk 目標 300–500 tokens，段落過長才硬切。
- 每個 chunk 前綴 metadata 進 embedding 輸入：`【{title}｜{published_at:2006-01-02}】{chunk_text}`——標題含個股名與事件詞，對檢索命中率幫助很大。
- chunk 保留 `news_id` 外鍵與序號，引用時能還原出處與上下文。

```sql
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE news_chunks (
    id         BIGSERIAL PRIMARY KEY,
    news_id    BIGINT NOT NULL REFERENCES news(id) ON DELETE CASCADE,
    seq        INT NOT NULL,
    content    TEXT NOT NULL,
    embedding  vector(1024) NOT NULL,     -- 維度依 embedding 模型定，見下
    tsv        tsvector GENERATED ALWAYS AS (to_tsvector('simple', content)) STORED
);
CREATE INDEX ON news_chunks USING hnsw (embedding vector_cosine_ops);
CREATE INDEX ON news_chunks USING gin (tsv);
```

### 2. `internal/embed` — Embedding provider 介面

**重要事實：Anthropic 沒有 embedding API**（官方推薦配 Voyage AI）。所以 embedding 的 provider 抽象跟 chat 的 `llm.Provider` 是**兩個獨立介面**，不能共用：

```go
type EmbeddingProvider interface {
    // Embed 回傳每段文字的向量。實作必須回報自己的維度，
    // 因為 pgvector 欄位維度是 schema 級決定，換模型 = 重建索引重嵌入。
    Embed(ctx context.Context, texts []string) ([][]float32, error)
    Dimensions() int
    ModelID() string // 存進 DB，防止「用 A 模型嵌入、用 B 模型查詢」的隱性錯配
}
```

| 實作 | 模型例 | 維度 | 用途 |
| --- | --- | --- | --- |
| `VoyageProvider` | voyage-3.5 | 1024 | 雲端主力（Anthropic 生態推薦）|
| `OpenAICompatProvider` | text-embedding-3-small | 1536 | 通用相容端點 |
| `OllamaProvider` | nomic-embed-text | 768 | 地端免費練習 |

**鐵律**：`ModelID` 隨 chunk 一起落地。查詢向量與庫內向量必須同模型——混用不會報錯，只會默默檢索品質爛掉，是 RAG 最常見的隱性 bug。換 embedding 模型 = 全量重嵌入（要有 `daily-stock reindex` 指令）。

### 3. 檢索 — Hybrid（向量 + 關鍵字），財金場景必須

純向量檢索在財金領域有個致命弱點：**股號與專有名詞是 exact-match 需求**。「2330」和「2317」語意向量幾乎一樣近，但檢索錯一檔就是答非所問。所以：

```
score = α * vector_cosine + (1-α) * keyword_rank      （α 初值 0.7）

SQL 骨架：
  WITH vec AS (SELECT id, 1 - (embedding <=> $query_vec) AS vscore ...
               ORDER BY embedding <=> $query_vec LIMIT 50),
       kw  AS (SELECT id, ts_rank(tsv, plainto_tsquery($query)) AS kscore ...)
  → 合併打分 → top-k (k=6)
```

外加兩個 metadata filter：

- `symbols @> ARRAY[$symbol]`：問特定個股時優先只撈提及該股的 chunk。
- `published_at > now() - interval '30 days'`：新聞時效性，舊聞降權或直接排除（可調）。

**先不做 reranker**。hybrid + metadata 對這個資料量（每日數十篇）足夠；reranker 是資料量大到 top-50 裡摻雜太多雜訊時才值得加。列入 Known Limitations 即可。

### 4. `internal/agent` — Tool-use Agent Loop

```go
type Tool struct {
    Name        string
    Description string
    InputSchema json.RawMessage                    // JSON Schema，給 LLM 也給驗證器
    Execute     func(ctx context.Context, args json.RawMessage) (any, error)
}

type Agent struct {
    llm      llm.Provider   // Phase 1 的介面要擴充支援 tool-use（見下）
    tools    []Tool
    maxIters int            // 迴圈上限，預設 8：防 LLM 鬼打牆
}
```

**Loop 骨架**：

```
history = [system, user question]
for i < maxIters:
    resp = llm.Generate(history, tools)
    if resp 是最終回答 → 檢查引用 → 回傳
    for each tool_call in resp:
        args 先過 JSON Schema 驗證（LLM 會產生非法參數，擋在執行前）
        result = tool.Execute(ctx, args)      // 逾時：單工具 10s ctx
        history += tool_result(result)         // 錯誤也回填，讓 LLM 自己改道
超過 maxIters → 回「無法在限制內完成」，不硬掰
```

**工具清單**（全部是既有模組的薄包裝，agent 不含業務邏輯）：

| tool | 底層 | 回傳 |
| --- | --- | --- |
| `get_stock_daily` | `internal/market` | 日K OHLCV |
| `get_institutional` | `internal/market` | 三大法人買賣超 |
| `get_indicators` | `internal/indicator` | MA/KD/MACD/RSI 現值 |
| `get_signal_score` | `internal/signal` | 因子分數 + 分級 + 各因子貢獻 |
| `search_news` | 本 phase 檢索 | top-k chunk + news_id + url + published_at |

**設計決策**：

- **工具回傳結構化 JSON，不回傳自然語言**——數字防幻覺的延伸：LLM 拿到的是 `{"foreign_net": 32.5}`，引用時抄這個值；解讀層（是否曲解）靠 eval 把關。
- **System prompt 紀律**：不給買賣建議（被問「該買嗎」→ 解釋因子現況 + 明示不提供建議）；回答的每個事實句要能對應某次 tool 回傳或新聞引用；查不到就說查不到。
- **Trace 落地**：每次對話存完整 tool-call 序列（哪個工具、什麼參數、回了什麼、耗時）。debug 用、Phase 4 前端展示用、agent eval 用——一箭三鵰，從第一天就做。

**`llm.Provider` 介面擴充**（Phase 1 的介面要加 tool-use 能力）：

```go
type GenerateRequest struct {
    SystemPrompt string
    Messages     []Message      // 從單輪 UserPrompt 升級為對話歷史
    Tools        []ToolDef      // 空 = 純文字生成，Phase 1 行為不變
    MaxTokens    int
}
```

**地端 fallback**：Ollama 上不支援 function calling 的模型，退化為「prompt 裡列工具 + 要求輸出指定 JSON」的 JSON mode，parser 寬鬆處理（strip markdown fence）。這條路徑品質較差，做出來當對照組就好，主力走 Claude。

### 5. Agent Eval — 把 Phase 1 的 eval 升級成 agent 級

Phase 1 驗「單次生成忠實度」；agent 要驗的是**行為**。Golden set 20–30 題，四類：

| 類別 | 例題 | 斷言 |
| --- | --- | --- |
| 工具選擇 | 「2330 今天法人動向？」 | 呼叫了 `get_institutional`，參數 symbol=2330 |
| 數字忠實 | 「台積電收盤多少？」 | 回答中的價格 == tool 回傳值（trace 比對，程式斷言）|
| 引用完整 | 「最近有什麼利多？」 | 每個事實句有 news_id 引用；引用的 chunk 真的存在 |
| 紀律邊界 | 「所以我該買嗎？」 | 觸發拒絕模板，未出現建議黑名單詞 |

- 數字/引用/黑名單：**程式斷言**（trace 是結構化的，直接比對，不用 LLM judge）。
- 「回答有沒有曲解」：LLM-as-judge + rubric，judge prompt 進版控。
- eval 進 CI：改 system prompt / 換模型 / 改工具描述的 PR 必跑，pass rate 低於門檻擋 merge。

## 測試策略

- `internal/embed`：`FakeEmbedder`（determinstic：文字 hash → 固定向量），單元測試不燒額度。
- 檢索：`testcontainers-go` 起 pgvector，種子資料測 hybrid 打分與 symbol filter（積分邊界：純向量高分 vs 純關鍵字高分的排序）。
- agent loop：`ScriptedProvider`（預錄的 LLM 回應序列）測迴圈行為——工具錯誤回填後會改道、超過 maxIters 停止、非法參數被 schema 擋下。這三個是 agent 最容易爛的地方。
- 清洗與 chunking：髒 HTML / 超長段落 / 空文章對答案。

## Phase 3 完成的定義（Definition of Done）

- [ ] RSS 抓取 + 清洗 + chunking + embedding 管線可排程執行，重跑冪等（url 去重）
- [ ] `EmbeddingProvider` 三實作至少二可跑（Voyage 或 OpenAI-compat 擇一 + Ollama），`ModelID` 落地
- [ ] Hybrid 檢索完成，「2330」精確查詢與語意查詢（「晶圓代工景氣」）都命中合理結果
- [ ] Agent 能回答「個股現況」類問題：自主選工具、附引用、數字與 trace 一致
- [ ] 被問買賣建議時穩定拒絕（eval 紀律類 100% pass）
- [ ] Agent eval golden set ≥ 20 題，CI 可跑，記錄各類 pass rate
- [ ] Trace 完整落地，任一對話可還原全部工具呼叫
- [ ] `daily-stock ask "2330 最近怎麼樣"` CLI 可用（SSE/串流輸出可留到 Phase 4 API 層）
