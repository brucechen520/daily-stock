package notify_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/brucechen520/daily-stock/internal/notify"
)

func TestPack_KeepsSmallPayloadInOneMessage(t *testing.T) {
	secs := []notify.Section{
		{Body: "大盤收 44,928.76 點"},
		{Body: "2330 台積電 +2.1%"},
	}

	got := notify.Pack(secs, 2000)

	if len(got) != 1 {
		t.Fatalf("訊息數 = %d, want 1（總長遠小於上限就別拆）", len(got))
	}
	for _, want := range []string{"大盤收", "2330"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("訊息缺少 %q：%s", want, got[0])
		}
	}
}

func TestPack_SplitsWhenExceedingLimit(t *testing.T) {
	secs := []notify.Section{
		{Body: strings.Repeat("甲", 60)},
		{Body: strings.Repeat("乙", 60)},
		{Body: strings.Repeat("丙", 60)},
	}

	got := notify.Pack(secs, 130)

	if len(got) < 2 {
		t.Fatalf("訊息數 = %d, want >= 2（超過上限要拆）", len(got))
	}
	for i, m := range got {
		if n := utf8.RuneCountInString(m); n > 130 {
			t.Errorf("第 %d 則長度 %d 字元超過上限 130", i+1, n)
		}
	}
}

// 上限的單位是字元不是 byte——中文一字 3 bytes，用 byte 計會多切三倍。
func TestPack_LimitCountsCharactersNotBytes(t *testing.T) {
	secs := []notify.Section{{Body: strings.Repeat("台", 90)}} // 90 字 = 270 bytes

	got := notify.Pack(secs, 100)

	if len(got) != 1 {
		t.Fatalf("訊息數 = %d, want 1（90 字未超過 100 字上限）", len(got))
	}
}

// 邊界：剛好等於上限不拆，多一個字就拆。
func TestPack_BoundaryAtExactLimit(t *testing.T) {
	exact := notify.Pack([]notify.Section{{Body: strings.Repeat("台", 2000)}}, notify.DiscordLimit)
	over := notify.Pack([]notify.Section{{Body: strings.Repeat("台", 2001)}}, notify.DiscordLimit)

	if len(exact) != 1 {
		t.Errorf("剛好 2000 字 → %d 則, want 1", len(exact))
	}
	if len(over) != 2 {
		t.Errorf("2001 字 → %d 則, want 2", len(over))
	}
}

// 單一 section 本身就超過上限時必須硬切，不能整段丟掉。
func TestPack_SplitsOversizedSingleSection(t *testing.T) {
	secs := []notify.Section{{Body: strings.Repeat("x", 250)}}

	got := notify.Pack(secs, 100)

	if len(got) != 3 {
		t.Fatalf("訊息數 = %d, want 3（250 字切 100 上限）", len(got))
	}
	if total := strings.Join(got, ""); len(total) != 250 {
		t.Errorf("切割後總長度 = %d, want 250（不可遺失內容）", len(total))
	}
}

// 中文是多 byte，硬切不可切在 rune 中間，否則 Discord 收到亂碼。
func TestPack_OversizedSectionSplitsOnRuneBoundary(t *testing.T) {
	secs := []notify.Section{{Body: strings.Repeat("台", 100)}}

	got := notify.Pack(secs, 40)

	if len(got) != 3 {
		t.Fatalf("訊息數 = %d, want 3（100 字切 40 字上限）", len(got))
	}
	for i, m := range got {
		if !utf8.ValidString(m) {
			t.Errorf("第 %d 則含無效 UTF-8：%q", i+1, m)
		}
	}
	if total := strings.Join(got, ""); total != strings.Repeat("台", 100) {
		t.Error("切割後內容與原文不一致")
	}
}

func TestPack_IgnoresEmptySections(t *testing.T) {
	got := notify.Pack([]notify.Section{{Body: ""}, {Body: "  "}, {Body: "有內容"}}, 2000)

	if len(got) != 1 {
		t.Fatalf("訊息數 = %d, want 1", len(got))
	}
	if strings.TrimSpace(got[0]) != "有內容" {
		t.Errorf("訊息 = %q, want %q（空 section 不該產生空白）", got[0], "有內容")
	}
}

func TestPack_AllEmptyProducesNoMessages(t *testing.T) {
	got := notify.Pack([]notify.Section{{Body: ""}}, 2000)

	if len(got) != 0 {
		t.Errorf("訊息數 = %d, want 0（沒內容就不該送）", len(got))
	}
}
