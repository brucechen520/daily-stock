// Package news 抓財金新聞 RSS、清洗、抽個股代號，交由 store 以 url 去重落地。
// Phase 1 只做「抓取落地」；chunking / embedding / 檢索是 Phase 3（docs/phase-3.md）。
package news

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
)

// Item 是清洗完、可入庫的一則新聞。
type Item struct {
	Source      string
	URL         string
	Title       string
	Body        string
	PublishedAt time.Time
	Symbols     []string
}

// Fetcher 抓一組 RSS feed。
type Fetcher struct {
	Feeds  []string
	parser *gofeed.Parser
	now    func() time.Time
}

func NewFetcher(feeds []string) *Fetcher {
	p := gofeed.NewParser()
	p.UserAgent = "daily-stock/0.1 (personal research)"
	return &Fetcher{Feeds: feeds, parser: p, now: time.Now}
}

// FetchAll 抓所有 feed 並清洗。單一 feed 失敗不擋其他 feed，錯誤彙整回傳。
func (f *Fetcher) FetchAll(ctx context.Context) ([]Item, error) {
	var items []Item
	var errs []string
	for _, feedURL := range f.Feeds {
		feed, err := f.parser.ParseURLWithContext(feedURL, ctx)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", feedURL, err))
			continue
		}
		src := sourceName(feedURL)
		for _, it := range feed.Items {
			item, ok := f.clean(src, it)
			if ok {
				items = append(items, item)
			}
		}
	}
	if len(errs) > 0 {
		return items, fmt.Errorf("部分 feed 失敗: %s", strings.Join(errs, "; "))
	}
	return items, nil
}

// clean 把一則 RSS item 洗成可入庫的 Item。缺 url/title 的丟棄。
func (f *Fetcher) clean(source string, it *gofeed.Item) (Item, bool) {
	link := strings.TrimSpace(it.Link)
	title := CleanText(it.Title)
	if link == "" || title == "" {
		return Item{}, false
	}
	body := CleanText(it.Content)
	if body == "" {
		body = CleanText(it.Description)
	}
	published := f.now()
	if it.PublishedParsed != nil {
		published = *it.PublishedParsed
	}
	text := title + " " + body
	return Item{
		Source:      source,
		URL:         link,
		Title:       title,
		Body:        body,
		PublishedAt: published.UTC(),
		Symbols:     ExtractSymbols(text),
	}, true
}

var (
	tagRe    = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRe  = regexp.MustCompile(`\s+`)
	symbolRe = regexp.MustCompile(`[（(](\d{4,6})(?:-KY)?[)）]`)
)

// CleanText 去 HTML tag、解 entity、壓空白。
func CleanText(s string) string {
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, " ", " ") // &nbsp; 解出來是 NBSP，Go regexp 的 \s 不含它
	s = spaceRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// ExtractSymbols 從文字抽個股代號。台媒慣例是「台積電(2330)」——
// 抓括號內 4-6 位數字，去重排序。啟發式,不求全,Phase 3 檢索的 symbol filter 用。
func ExtractSymbols(text string) []string {
	seen := map[string]bool{}
	for _, m := range symbolRe.FindAllStringSubmatch(text, -1) {
		seen[m[1]] = true
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// sourceName 從 feed URL 推來源名（ltn / cnyes / …），失敗回 host。
func sourceName(feedURL string) string {
	u, err := url.Parse(feedURL)
	if err != nil {
		return feedURL
	}
	host := u.Hostname()
	parts := strings.Split(host, ".")
	// news.ltn.com.tw → ltn；news.cnyes.com → cnyes
	if len(parts) >= 3 && parts[len(parts)-1] == "tw" {
		return parts[len(parts)-3] // xxx.com.tw / xxx.org.tw 取倒數第三段
	}
	if len(parts) >= 2 {
		return parts[len(parts)-2]
	}
	return host
}
