# Phase 1 — 盤後白話摘要 MVP

## 目標

輸入當日 TWSE 大盤指數 + 三大法人買賣超 + 個股日K，經由 LLM 生成**白話解讀 + 術語註解**。**不產生任何買賣訊號**，純粹是「今天發生了什麼、代表什麼意思」的教學型摘要。同時建立 provider 可切換架構與 LLM 輸出忠實度驗證機制，作為後續 phase 的地基。

## 產出範例（示意）

```
📊 2026-08-07 盤後摘要

大盤：加權指數收 22,150 點，漲 85 點（+0.39%）
→ 白話：今天大盤小漲，屬於溫和整理格局，沒有明顯方向性表態。

三大法人：外資買超 32 億元，投信賣超 5 億元
→ 白話：外資站買方，是短線偏多的訊號之一，但投信同步賣超，
   顯示法人對後市看法不完全一致。

【術語小教室】
買超/賣超：指法人當日買進金額扣掉賣出金額後的淨額，
正值代表淨買進（偏多訊號），負值代表淨賣出（偏空訊號）。

⚠️ 以上內容僅供學習與理解市場動態使用，非投資建議。
```

## 子模組拆解

### 1. `internal/market` — 資料源介面 + TWSE client

```go
type MarketDataProvider interface {
    GetIndexDaily(ctx context.Context, date time.Time) (*IndexData, error)
    GetInstitutionalTrading(ctx context.Context, date time.Time) (*InstitutionalData, error)
    GetStockDaily(ctx context.Context, symbol string, date time.Time) (*StockDaily, error)
}
```

- `TWSEClient`：實作真實 API 呼叫（TWSE OpenAPI 為 JSON REST，免金鑰但有 rate limit，建議加上重試與 cache）
- `MockClient`：回傳固定測試資料，方便本地開發與單元測試不依賴外部 API
- 兩者都實作同一 interface，上層完全不用改

**注意事項**：
- TWSE OpenAPI 資料是「盤後」才更新，開發時注意抓取時間點（通常收盤後 1-2 小時資料才完整）
- 建議把原始回應落地存成 JSON（例如存到本地檔案或 SQLite），除了 debug 方便，也是 Phase 2 回測要用的歷史資料

### 2. `internal/llm` — Provider 介面設計

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

- `AnthropicProvider`：呼叫 Claude API
- `OllamaProvider`：呼叫本地 Ollama（OpenAI-compatible 端點）
- `factory.go`：依 config 的 `provider: "anthropic" | "ollama"` 回傳對應實作

這樣設計的好處是，`internal/report` 完全不需要知道底層是哪個 provider，換模型只改一行 config。

### 3. `internal/report` — Prompt 組裝與防幻覺設計

**核心原則：LLM 只能看到「已經算好的真實數字」，不能自己編數字。**

Prompt 設計範例：

```
你是台股市場的說明員，任務是把以下真實數據解讀成白話文，並穿插術語教學。

嚴格規則：
1. 只能使用下方提供的數字，不可自行估算或編造任何數值
2. 若某項數據缺失，直接說明「今日無相關資料」，不可推測
3. 不可給出買賣建議，只能解釋「這代表什麼現象」
4. 每次至少挑一個術語做簡短教學

今日數據：
- 加權指數：{index_close}，漲跌：{index_change}（{index_change_pct}%）
- 外資買賣超：{foreign_net} 億元
- 投信買賣超：{investment_trust_net} 億元
- 自營商買賣超：{dealer_net} 億元

請用 200-300 字生成白話摘要。
```

**關鍵技巧**：把「規則」和「數據」明確分區塊，並且用「嚴格規則」開頭列點，比起把規則寫成一句話混在敘述裡，LLM 遵守度會明顯較高。

### 4. `internal/glossary` — 術語辭典

簡單版本：一份 YAML/JSON 對照表（術語 → 白話解釋），report 模組生成時隨機挑一個當日數據有出現的術語塞進 prompt context，讓 LLM 教學內容有依據可循，不會亂掰解釋。

```yaml
- term: "外資買超"
  explanation: "外國機構投資人當日買進金額大於賣出金額，通常視為對台股後市偏多的訊號之一"
- term: "投信"
  explanation: "國內基金公司，買賣行為常反映法人機構型投資人對個股基本面的看法"
```

### 5. `internal/eval` — LLM 輸出忠實度驗證（新增，Phase 1 起）

這是這次調整新加的模組，目的是驗證「LLM 有沒有亂講數字或曲解意思」。

**做法**：
1. 手動準備 20-30 組測試案例：`{輸入數據} → {正確理解要點}`（例如：外資買超 32 億 → 應該被理解為「偏多訊號」，不能被說成「賣超」或誇大成「主力大舉進場」）
2. 每次跑完 report 生成後，用簡單規則或另一次 LLM 呼叫（as judge）比對輸出是否：
   - 數字與輸入一致（可用正則抓數字直接比對，比 LLM judge 更可靠）
   - 沒有出現「買賣建議」相關用詞（可維護一份黑名單詞彙，如「建議買進」、「應該賣出」）
3. 每次改 prompt 或換 provider，重跑這組 eval，記錄 pass rate

```go
type EvalCase struct {
    Input          MarketSnapshot
    ExpectedNumbers []string // 應該出現在輸出中的關鍵數字
    ForbiddenPhrases []string // 不應出現的建議性字眼
}
```

這個機制不用做得複雜，但**一定要有**——它是整個專案「把 LLM 當不完全可信元件在管理」的具體證明，面試會被問到「怎麼確保 LLM 沒有亂講」，這就是你的答案。

## CLI 使用流程

```bash
# 抓當日資料並生成摘要
daily-stock summary --date 2026-08-07

# 指定 provider
daily-stock summary --date 2026-08-07 --provider ollama

# 跑 eval
daily-stock eval --provider anthropic
```

## 測試策略

- `internal/market`：用 MockClient 測試資料解析邏輯，不呼叫真實 API
- `internal/llm`：用假的 HTTP server 測試 request/response 組裝，不燒 API 額度
- `internal/report`：測試 prompt 組裝是否正確帶入真實數字（字串比對）
- `internal/eval`：這本身就是測試工具，但也要為 eval 邏輯本身寫單元測試（例如數字比對函式）

## Phase 1 完成的定義（Definition of Done）

- [ ] TWSE 大盤 + 三大法人資料可正確抓取並落地
- [ ] `llm.Provider` 介面完成，Claude 與 Ollama 至少一個能實際跑通
- [ ] 白話摘要 CLI 可產出上述範例格式的輸出
- [ ] 術語教學至少涵蓋 10 個常見詞彙
- [ ] eval set 至少 20 組案例，且有一個可重複執行的 eval 指令
- [ ] README 附上真實 demo 輸出截圖或範例