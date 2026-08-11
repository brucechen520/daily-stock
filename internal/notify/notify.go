// Package notify 是推播層，刻意切成兩半：
//
//	render（別的 package 產內容）→ []Section
//	sender（這裡）             → 把 Section 塞進通道限制並送出
//
// 這條縫的用途：之後改成「只列異動檔」或改用網頁，只動 render；
// 換通道（email / LINE）只動 sender。兩邊不互相知道對方的細節。
package notify

import (
	"strings"
	"unicode/utf8"
)

// Section 是一段語意完整的內容（大盤、持股、新聞、術語…）。
// Sender 會盡量讓同一個 Section 待在同一則訊息裡。
type Section struct {
	Body string
}

// DiscordLimit 是 Discord 單則訊息的上限，單位是**字元**不是 byte。
// 這個區別對中文很要命：2000 字的中文摘要是 6000 bytes，
// 用 byte 計數會把本來一則裝得下的內容切成三則。
const DiscordLimit = 2000

// Pack 把 Section 併成數則訊息，每則不超過 limit 個字元。
// 規則：能塞就塞同一則；單一 Section 自己就超長時硬切（切在 rune 邊界）。
func Pack(sections []Section, limit int) []string {
	const sep = "\n\n"
	sepLen := utf8.RuneCountInString(sep)

	var msgs []string
	var cur strings.Builder
	curLen := 0 // cur 的字元數（strings.Builder 只給 byte 數）

	flush := func() {
		if curLen > 0 {
			msgs = append(msgs, cur.String())
			cur.Reset()
			curLen = 0
		}
	}

	for _, s := range sections {
		body := strings.TrimSpace(s.Body)
		if body == "" {
			continue
		}
		for _, chunk := range splitOnRune(body, limit) {
			n := utf8.RuneCountInString(chunk)
			need := n
			if curLen > 0 {
				need += sepLen
			}
			if curLen+need > limit {
				flush()
			}
			if curLen > 0 {
				cur.WriteString(sep)
				curLen += sepLen
			}
			cur.WriteString(chunk)
			curLen += n
		}
	}
	flush()
	return msgs
}

// splitOnRune 把過長字串切成每段 <= limit 個字元，不切在 rune 中間。
// 未超長時原樣回傳（單元素），呼叫端不必分支。
func splitOnRune(s string, limit int) []string {
	if utf8.RuneCountInString(s) <= limit {
		return []string{s}
	}
	var out []string
	for utf8.RuneCountInString(s) > limit {
		cut, n := 0, 0
		for cut < len(s) && n < limit {
			_, size := utf8.DecodeRuneInString(s[cut:])
			cut += size
			n++
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}
