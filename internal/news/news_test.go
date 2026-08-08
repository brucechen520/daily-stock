package news_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/brucechen520/daily-stock/internal/news"
)

func TestCleanText_StripsHTMLAndUnescapesEntities(t *testing.T) {
	dirty := `<p>台積電&nbsp;法說會<br/>釋出&quot;樂觀&quot;訊號</p>  <script>ad()</script> 延伸閱讀`

	got := news.CleanText(dirty)

	if want := `台積電 法說會 釋出"樂觀"訊號 ad() 延伸閱讀`; got != want {
		t.Errorf("CleanText = %q, want %q", got, want)
	}
}

func TestExtractSymbols_FindsCodesInParensBothWidths(t *testing.T) {
	text := "台積電（2330）帶動大盤，鴻海(2317)同步走揚；外資喊到 1200 元。信驊(5274)創高。"

	got := news.ExtractSymbols(text)

	// 1200 沒括號不該被抓；結果排序去重
	if want := []string{"2317", "2330", "5274"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractSymbols = %v, want %v", got, want)
	}
}

func TestExtractSymbols_NoMatchReturnsEmpty(t *testing.T) {
	got := news.ExtractSymbols("大盤上漲 266 點，成交量 8850 億元")

	if len(got) != 0 {
		t.Errorf("ExtractSymbols = %v, want 空（裸數字不是代號）", got)
	}
}

const rssFixture = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>財經新聞</title>
<item>
  <title><![CDATA[台積電(2330)法說會報喜]]></title>
  <link>https://news.example.com.tw/a/1</link>
  <description><![CDATA[<p>台積電&nbsp;釋出樂觀展望</p>]]></description>
  <pubDate>Fri, 07 Aug 2026 10:00:00 +0800</pubDate>
</item>
<item>
  <title>沒有連結的壞資料</title>
  <link></link>
</item>
</channel></rss>`

func TestFetchAll_ParsesCleansAndSkipsBrokenItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(rssFixture))
	}))
	defer srv.Close()

	f := news.NewFetcher([]string{srv.URL})
	items, err := f.FetchAll(context.Background())

	if err != nil {
		t.Fatalf("FetchAll 回傳非預期錯誤: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1（缺 link 的要跳過）", len(items))
	}
	got := items[0]
	if want := "台積電(2330)法說會報喜"; got.Title != want {
		t.Errorf("Title = %q, want %q", got.Title, want)
	}
	if want := "台積電 釋出樂觀展望"; got.Body != want {
		t.Errorf("Body = %q, want %q（HTML 要洗掉）", got.Body, want)
	}
	if want := []string{"2330"}; !reflect.DeepEqual(got.Symbols, want) {
		t.Errorf("Symbols = %v, want %v", got.Symbols, want)
	}
	if got.PublishedAt.IsZero() {
		t.Error("PublishedAt 是零值, want pubDate 解析結果")
	}
}

func TestFetchAll_OneFeedFailureDoesNotDropOthers(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(rssFixture))
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	f := news.NewFetcher([]string{bad.URL, good.URL})
	items, err := f.FetchAll(context.Background())

	if err == nil {
		t.Error("有 feed 失敗應回錯誤讓呼叫端知道, got nil")
	}
	if len(items) != 1 {
		t.Errorf("len(items) = %d, want 1（好 feed 的結果不能被壞 feed 拖掉）", len(items))
	}
}
