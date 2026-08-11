# Phase 1.5 — 自選股每日追蹤（自用版）

> 本文件記錄一次**方向轉折**：專案的第一優先從「求職作品集」改為「我自己每天真的會用的工具」。
> 這個轉折改變了 phase 順序（watchlist / 推播插隊到回測 phase 2' 之前）與驗收標準（從「DoD 打勾」變成「我明天會不會打開它」）。
> 決策過程見 §1，工程規格見 §2 之後。

---

## 1. 為什麼有這個 phase

### 1.1 北極星重排

| 優先序 | 目標 | 影響 |
| --- | --- | --- |
| **1** | **自己每天實際使用**（我是真實持股人）| 產品決策的仲裁者 = 「我明天會不會打開它」 |
| 2 | 未來可能對外的產品（PRD 情境）| 現在不做，但不做出擋路的設計（例如持倉資料不進版控）|
| 3 | AI 工程練習（RAG / agent / eval）| 仍是 Phase 3 的主菜，順序不變 |

原 ROADMAP 隱含的優先序是「作品集」，所以順序是 **2'(回測) → 3(RAG/agent) → 4(web)**。改為自用後，最痛的需求變成「**我手上這 20 檔今天怎麼了**」，而那不在任何既有 phase 裡——它橫跨 phase 1 的尾巴、phase 2' 的一部分、phase 4 的一部分。與其扭曲既有 phase，獨立成 Phase 1.5。

### 1.2 這個 phase 的一句話

**每個交易日收盤後，把「大盤 + 我持有的 20 檔今天發生什麼 + 相關新聞 + 一個術語」推到我手機上。**

### 1.3 明確不做（守住既有紅線）

- ❌ **不出訊號、不做回測**：本 phase 全部是**描述性統計**（把公開數字換算成人看得懂的形式），不是訊號。ROADMAP 原則 4「回測站得住才出訊號」不動搖，回測仍在 BACKLOG。
- ❌ **不讓 LLM 讀新聞**（理由見 §5.3）
- ❌ **不讓 LLM 看到成本與損益**（理由見 §5.3）
- ❌ 不做網頁（Phase 4 不變）、不做問答 agent（Phase 3 不變）

---

## 2. 資料層

### 2.1 新增資料源（皆已實測可用）

| 資料 | 端點 | 歷史 | 用途 |
| --- | --- | --- | --- |
| 個股日K（歷史）| `www.twse.com.tw/exchangeReport/STOCK_DAY?response=json&date=YYYYMMDD&stockNo=2330` | **一請求 = 該股一整月** | 回補 12 個月，MA/量能/高低點需要 |
| 個股三大法人 | `www.twse.com.tw/fund/T86?response=json&date=YYYYMMDD&selectType=ALL` | **一請求 = 該日全市場** | 個股外資/投信/自營商買賣超、連買連賣天數 |
| 除權息計算結果 | `www.twse.com.tw/exchangeReport/TWT49U?response=json&date=YYYYMMDD` | 一請求 = 該日 | 還原因子、現金股利 |

> **關鍵事實修正**：Phase 1 文件說「TWSE 多數端點只回最新交易日」，那是 **OpenAPI（`openapi.twse.com.tw`）** 的限制。上述 **RWD 端點（`www.twse.com.tw`）帶 `date` 參數可取歷史**，所以 MA60 不必等排程累積三個月，直接回補即可。既有的 `daily-stock backfill` 已經走 RWD 路徑。
>
> `TWT49U` 直接回「除權息前收盤價」與「除權息參考價」，**TWSE 已經把現金股息與配股一併算進參考價**，所以還原因子不必自己拆解各種公司行動。減資／增資不在這張表，罕見，走標記處理。

### 2.2 Schema（新 migration `002_watchlist_and_adjust.sql`）

```sql
-- +goose Up

-- 自選股 + 持倉（自用；資料不進版控，見 §7）
CREATE TABLE watchlist (
    symbol     TEXT PRIMARY KEY,
    shares     BIGINT NOT NULL DEFAULT 0,        -- 0 = 只觀察不持有
    avg_cost   NUMERIC(10,2),                    -- 均價；分批買進自行算好再更新
    bought_at  DATE,                             -- 首次買進日；判定領到哪幾次配息
    note       TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 個股三大法人（T86，單位：股）
CREATE TABLE institutional_stock_daily (
    symbol      TEXT NOT NULL,
    trade_date  DATE NOT NULL,
    foreign_net BIGINT NOT NULL,                 -- 外陸資（不含外資自營商）買賣超股數
    trust_net   BIGINT NOT NULL,                 -- 投信
    dealer_net  BIGINT NOT NULL,                 -- 自營商合計
    total_net   BIGINT NOT NULL,                 -- 三大法人合計
    PRIMARY KEY (symbol, trade_date)
);

-- 公司行動：除權息（TWT49U）
CREATE TABLE corporate_actions (
    symbol     TEXT NOT NULL,
    ex_date    DATE NOT NULL,                    -- 除權息交易日
    kind       TEXT NOT NULL,                    -- '息' / '權' / '權息'
    prev_close NUMERIC(10,2) NOT NULL,           -- 除權息前收盤價
    ref_price  NUMERIC(10,2) NOT NULL,           -- 除權息參考價
    value      NUMERIC(10,4) NOT NULL,           -- 權值+息值
    adj_factor NUMERIC(12,8) NOT NULL,           -- ref_price / prev_close
    PRIMARY KEY (symbol, ex_date)
);

-- 還原收盤價：物化 cache（失效規則見 §3.1）
ALTER TABLE stock_daily ADD COLUMN adj_close NUMERIC(10,2);
```

**冪等鐵律沿用**：全部 `INSERT ... ON CONFLICT ... DO UPDATE`。

### 2.3 回補規模與速率

| 項目 | 請求數 | 說明 |
| --- | --- | --- |
| 個股日K：20 檔 × 12 個月 | 240 | 一請求一個月 |
| T86：60 個交易日 | 60 | 一請求一天，全市場落地後才過濾 watchlist |
| TWT49U：12 個月 | ≈ 250 | 逐交易日；台股除權息集中 6–8 月 |

全部走 rediskit `RateLimiter`（沿用 Phase 1 §2.4 紀律）。RWD 端點對密集請求會擋，回補間隔放寬到 3–5 秒，總計約 30–45 分鐘，一次性作業。

### 2.4 排程調整

```
17:30 平日   ingest market   大盤 + 法人總額 + 全市場日K（既有）
17:45 平日   ingest stock    T86 個股法人 + TWT49U 除權息（新增）
18:30 平日   generate+push   摘要生成 → Discord 推播（新增推播）
*/30 全週    ingest news     RSS（既有）
```

非交易日 TWSE 回空 → 記 log 跳過，不推播（§5.2）。

---

## 3. 計算層（`internal/indicator`）

### 3.1 還原股價

```
adj_factor(ex_date) = 除權息參考價 ÷ 除權息前收盤價
adj_close(d) = close(d) × Π adj_factor(e)   for all ex_date e > d
```

**存法**：`corporate_actions` 存因子（真相來源）+ `stock_daily.adj_close` 物化（查詢用）。

> **失效規則（唯一容易出錯的地方）**：寫入 `corporate_actions` 某檔新紀錄時，**必須在同一個 transaction 內重算該檔全部歷史 `adj_close`**。不可交給 cron 事後補——漏了就是靜默的錯誤資料，而錯誤資料比沒資料更糟。

### 3.2 統計項目（全部用 `adj_close` 計算）

| 項目 | 定義 |
| --- | --- |
| 漲跌幅 | 近 1 / 5 / 20 日 |
| 量能比 | 當日成交股數 ÷ 20 日均量 |
| 均線 | MA5 / MA20 / MA60 與股價相對位置（上/下、乖離%）|
| 相對高低 | 距 60 日最高 / 最低的百分比 |
| 法人連買連賣 | 外資、投信各自的連續買超（賣超）天數 |

### 3.3 報酬率：兩個都算，因為它們是不同的東西

| 欄位 | 公式 | 意義 |
| --- | --- | --- |
| **價格報酬** | `(現價 − avg_cost) ÷ avg_cost` | 只看價差。高股息股會系統性偏低 |
| **總報酬** | 價格報酬 + `Σ 現金股利(ex_date > bought_at) ÷ avg_cost` | 含你實際領到的配息 |

- 現金股利取自 `corporate_actions` 中 `kind = '息'` 的 `value`。
- `kind` 含「權」（配股）者，總報酬**只計現金部分**並在欄位旁標「含配股，未計入股票股利」——誠實揭露優於偷偷算錯。
- **注意**：報酬率用的是**未還原的實際成交價與成本**，不是 `adj_close`。還原價是給技術分析（MA、漲跌幅）用的，拿還原價去比原始成本會得到第三個數字，那個數字兩邊都不是。這三個概念（價格報酬 / 總報酬 / 還原股價）一併進 glossary。

### 3.4 新聞關聯

`internal/news.ExtractSymbols` 目前只認台媒的「台積電**(2330)**」括號慣例，多數標題不帶代號。新增：**用 watchlist 這 20 檔的股名（`stock_daily.name`）比對新聞標題**。

- 只比對 watchlist 20 檔，不比對全市場——全市場 1000+ 股名會炸出大量誤判（「大同」「統一」「精華」本身就是普通中文詞）。
- 只比對標題，不比對內文——內文提及多半是順帶一提，不是該則新聞的主角。
- **已知風險**：若 watchlist 含常用詞股名仍會誤中。上線後抓一批誤判樣本檢視，再決定要不要加排除規則（例如要求標題同時出現產業關鍵字）。

---

## 4. 輸出層

### 4.1 render / sender 分層（重要）

```
render  → []Section{大盤+歸納, 持股清單, 新聞, 術語+免責}   ← 只管產內容
sender  → 把 Section 塞進通道限制（Discord 單則 2000 字元）  ← 只管怎麼送
```

20 檔逐檔一行約 55–70 字，加總會超過 Discord 單則 2000 字元上限，因此**分 3–4 則連續送**。之後若改成「只列異動檔」或改用網頁，只動 `render`，`sender` 一行不改。

### 4.2 推播內容

- **大盤段**：沿用 Phase 1 的模板代入摘要 + LLM 跨檔位歸納一段
- **持股段**：每檔一行，**純模板規則生成（零 LLM）**
  例：`2330 台積電  +2.1%  量 1.8x  MA20 上方 +3.2%  外資連買 3 日  ｜ 價格報酬 +12.3% / 總報酬 +15.1%`
- **新聞段**：每檔最多 2 則，只有標題 + 連結，範圍為上一個交易日收盤後至今
- **術語段**：優先挑當日輸出中實際出現的術語（PRD FR-3.2），沒命中才 fallback 輪播；+ 免責聲明

### 4.3 推播紀律

| 情境 | 行為 |
| --- | --- |
| 非交易日 | 不推 |
| 交易日但持股無異動 | 推極簡版（「今日大盤 X，持股無明顯異動」）—— **不可靜默**，否則分不清「今天沒事」與「排程死了」|
| ingest / LLM 失敗 | 推一則失敗訊息（例：`8/10 ingest 失敗：TWSE 回 503`）|
| 送到第 2 則時通道掛掉 | 重試 3 次，仍失敗則推一則「推播不完整」；**不重送整批**（重複訊息洗頻道比缺一則更煩）|

### 4.4 LLM provider

主用 **Gemini**（`LLM_PROVIDER=gemini`，adapter 與 config 已存在於 `internal/llm/openai_compat.go` 與 `internal/config/config.go`，零新程式碼）。Anthropic / Ollama 保留手動對照，維持 ROADMAP 原則 3。

---

## 5. M7 防線在本 phase 的延伸

### 5.1 逐檔敘述不經 LLM

個股的敘述本來就是規則化的（「漲 2.1%，量能 1.8 倍，外資連買 3 天」），交給 LLM 只是花錢買幻覺風險。LLM 的價值在跨檔位歸納——那是規則寫不出來的。

### 5.2 eval 擴充

現有 20 組全是大盤摘要。新增 **5–8 組持股歸納 case** + 擴充建議性字眼黑名單。

理由不是法遵（自用不對外，不觸法），是**使用者本人會被影響**：持有人每天讀一段 AI 寫的「這檔動能轉強」，久了會影響決策，而那段話沒有任何回測支撐——正是 ROADMAP 原則 4 要防的東西。防線是為自己設的。

### 5.3 兩條 prompt 禁令（硬性，寫成欄位白名單）

| 禁令 | 理由 |
| --- | --- |
| **新聞不進 prompt** | 新聞一進 prompt，LLM 就會編因果（「因為法說會展望樂觀，所以外資買超」）。這種句子讀起來完全合理、無法用 eval 驗證對錯，且對持有人的決策影響極大。新聞原樣列出，因果由使用者自己建立——這才是「理解 + 研究」的定位。Phase 3 有 RAG 引用溯源機制後再開。 |
| **成本 / 損益不進 prompt** | LLM 一旦看到「你套牢 12%」，寫出的解讀會不自覺往安慰或建議偏。 |

實作：`report` 層維護欄位白名單，`Validate` 對佔位符已有檢查，擴充為「白名單外欄位不得出現在餵給 LLM 的資料裡」。

---

## 6. 三週里程碑（垂直切片）

預算 30hr/週 × 3 週 = 90hr，估時 75hr，留 15hr 給意外（TWSE 民國年日期解析、T86 欄位對不上、回補被限速——這類事一定會發生）。

**採垂直切片而非瀑布**：第 1 週末就要有能動的推播。驗收標準含「我連續 N 天真的打開來讀」，那是需求驗證；瀑布會把它推到第 15 天才開始，萬一發現「其實我只想看大盤」，中間差 7 天的沉沒成本。

### 週 1 — 能動的最小推播（約 25h）

- [x] `002_watchlist_and_adjust.sql`：`watchlist` 表（+ `institutional_stock_daily`、`corporate_actions`、`adj_close` 一併建好，本週只用 watchlist）
- [x] `.gitignore` 擋掉 `data/*.local.sql` + repo 內留 `data/positions.example.sql`
- [ ] **`data/positions.local.sql` 建 20 檔持倉**（需要真實持股清單）
- [ ] **20 檔 × 12 個月日K backfill 執行 + 抽驗資料正確**（同上，需要清單）
- [x] `internal/indicator`：當日漲跌幅（最小集）
- [x] `internal/notify`：Discord webhook sender + render/sender 分層 + 多則切割（上限以**字元**計，中文才不會被多切三倍）
- [x] `internal/digest`：render 層（讀 pg → 組 Section），與 `ingest` 對稱
- [x] `daily-stock push [--date] [--dry]`、`daily-stock watch list|add|remove`
- [x] 排程 18:30 接上推播（`daily-digest` job）
- [x] **本週推播結尾標註**：`⚠️ 未還原股價，除權息日數字失真`（第 3 週拿掉）
- [ ] **`DISCORD_WEBHOOK_URL` 填值**（需要 webhook 網址）

**週 1 末交付**：大盤摘要 + 20 檔當日漲跌幅，每天自動進 Discord。

#### 週 1 實作與 spec 的差異（已知取捨）

| 項目 | 差異 | 理由 |
| --- | --- | --- |
| §4.3「非交易日不推」 | 改成推一則「今日盤後資料尚未到位（非交易日，或 TWSE 尚未更新）」 | 單靠 DB 分不出「非交易日」與「ingest 失敗」，而後者靜默的代價高得多。實測 TWSE OpenAPI 16:00 仍停在前一交易日，這條路徑會常走到。接上交易日曆後再收斂成不推。|
| 「無異動」門檻 | 定為漲跌幅 ±3%；查無資料視為有異動 | spec 未定義。週 2 補上量能比與法人連買後，那些也會成為異動條件（單看漲跌幅會漏掉量爆價平）。|
| 摘要落地 | `digest.Build` 負責寫 `summaries`，`summary` 指令改走同一條路 | 落地若留在呼叫端，排程漏寫一整週都不會有人發現。|

### 週 2 — 加厚成真正的追蹤（約 24h）

- [ ] T86 fetcher + `institutional_stock_daily` + 排程 + 回補 60 交易日
- [ ] `internal/indicator` 補齊：MA5/20/60 與乖離、量能比、距 60 日高低、法人連買連賣天數
- [ ] 新聞股名比對（§3.4）+ 誤判樣本檢視
- [ ] 推播持股段擴充為完整模板行 + 新聞段
- [ ] LLM 跨檔位歸納段（Gemini）+ 欄位白名單（§5.3）

**週 2 末交付**：推播內容完整，唯數字仍未還原。

### 週 3 — 正確性與防線（約 24h）

- [ ] TWT49U fetcher + `corporate_actions` + 回補 12 個月
- [ ] 還原因子計算 + `adj_close` 物化 + **失效重算**（§3.1）
- [ ] 所有統計改用 `adj_close`；**拿掉未還原警語**
- [ ] 報酬率：價格報酬 + 總報酬（§3.3）
- [ ] glossary 擴到約 20 詞（含「價格報酬 / 總報酬 / 還原股價 / 量能 / 均線 / 乖離 / 連買連賣」）+ 命中優先挑選
- [ ] eval 加 5–8 組持股歸納 case + 擴黑名單
- [ ] **Phase 1 DoD 收尾**：`internal/store` testcontainers 冪等測試、README demo 輸出與排程 log
- [ ] `BACKLOG.md` 更新

---

## 7. 隱私與版控

持倉成本與股數是敏感資料，且本 repo 未來可能公開（北極星第 2 順位）。git 歷史裡的持倉資料清不掉（要 rewrite history + force push），事前防範是零成本，事後補救不是。

- `.gitignore` 目前**只有 `.env`** → 新增 `data/positions.local.sql`、`data/*.local.sql`
- repo 內留 `data/positions.example.sql` 範本（假資料）
- 推播只顯示**報酬率百分比**，不顯示金額

---

## 8. 測試策略（延續 Phase 1 §4）

| 對象 | 方式 |
| --- | --- |
| `internal/store` | testcontainers 真 pg：upsert 冪等、`adj_close` 重算後結果一致（重算兩次結果相同）|
| T86 / TWT49U 解析 | `httptest` + 錄下來的真實 response fixture（含民國年日期、空回應、全形空白欄位）|
| 還原因子 | 手算 golden case：已知某檔除息前後價格，驗算 `adj_close` 連續性 |
| 報酬率 | golden case：含息 / 不含息 / 買進日在除息後（不該計入）三種情境 |
| 新聞股名比對 | 誤判樣本集：常用詞股名（大同、統一）不該被無關新聞命中 |
| `render` / `sender` | render 產出的 Section 內容正確性；sender 對 2000 字元邊界的切割（剛好 2000 / 2001 字元）|
| 欄位白名單 | 斷言成本、損益、新聞欄位不出現在餵給 LLM 的 payload 中 |

---

## 9. Definition of Done

- [ ] 連續 **5 個交易日**自動推播成功，數字抽驗正確（對照 TWSE 網站）
- [ ] 除權息還原正確：至少一檔在回補區間內除息的持股，其 MA 與漲跌幅無假跳空
- [ ] 價格報酬 / 總報酬 兩欄皆顯示且定義寫進 glossary
- [ ] 非交易日不推、無異動推極簡版、失敗有推
- [ ] eval 含持股歸納 case，`daily-stock eval` 通過
- [ ] 成本 / 損益 / 新聞皆不出現在 LLM prompt（有測試斷言）
- [ ] `data/positions.local.sql` 不在版控內
- [ ] Phase 1 遺留 DoD（testcontainers、README demo）補齊
- [ ] **我連續 5 個交易日真的打開來讀**（需求驗證，比上面全部更重要）

> 最後一項是這個 phase 的真正驗收。做完卻三天就不看了，該砍的不是功能而是方向——早知道比晚知道好。
