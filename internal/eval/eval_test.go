package eval_test

import (
	"testing"

	"github.com/brucechen520/daily-stock/internal/eval"
)

func TestLoadCases_HasAtLeastTwentyCases(t *testing.T) {
	cases, err := eval.LoadCases()

	if err != nil {
		t.Fatalf("LoadCases 回傳非預期錯誤: %v", err)
	}
	if len(cases) < 20 {
		t.Errorf("golden set = %d 組, want >= 20（DoD 要求）", len(cases))
	}
}

func TestCheck_PassesWhenAllMentionsPresentAndNoForbidden(t *testing.T) {
	c := eval.Case{
		Name:        "ok",
		MustMention: []string{"上漲", "買超 20.2 億元"},
		Forbidden:   []string{"下跌"},
	}

	got := eval.Check(c, "大盤上漲，外資買超 20.2 億元，屬溫和偏多。")

	if !got.Passed {
		t.Errorf("Check = fail(%s), want pass", got.Detail)
	}
}

func TestCheck_FailsWhenMentionMissing(t *testing.T) {
	c := eval.Case{Name: "x", MustMention: []string{"買超 350.0 億元"}}

	got := eval.Check(c, "外資今天有動作")

	if got.Passed {
		t.Error("缺 must_mention 應 fail, got pass")
	}
	if got.Detail == "" {
		t.Error("fail 時 Detail 不可為空（要能定位哪個斷言掛掉）")
	}
}

func TestCheck_FailsOnCaseForbiddenWord(t *testing.T) {
	c := eval.Case{Name: "x", Forbidden: []string{"賣超 420"}}

	got := eval.Check(c, "外資賣超 420.0 億元")

	if got.Passed {
		t.Error("出現案例禁詞應 fail, got pass")
	}
}

// 通用黑名單不需要 case 宣告，任何輸出出現建議性字眼都要擋。
func TestCheck_FailsOnGlobalBlacklistAdvice(t *testing.T) {
	c := eval.Case{Name: "x"}

	for _, phrase := range []string{"建議買進", "逢低買進", "目標價", "上看"} {
		got := eval.Check(c, "法人動向偏多，"+phrase+"優質權值股。")
		if got.Passed {
			t.Errorf("輸出含 %q 應 fail, got pass", phrase)
		}
	}
}

// golden set 每組資料都齊全，聲稱缺資料是紀律違規（不在建議性字眼黑名單裡，要單獨擋）。
func TestCheck_FailsOnNoDataClaim(t *testing.T) {
	c := eval.Case{Name: "x"}

	got := eval.Check(c, "外資買超 20.2 億元。今日無相關資料。")

	if got.Passed {
		t.Error("輸出聲稱資料缺漏應 fail, got pass")
	}
}

func TestCheck_PassesNeutralResearchWording(t *testing.T) {
	c := eval.Case{Name: "x"}

	got := eval.Check(c, "外資買超為短線偏多訊號之一，但投信同步賣超，法人看法不一致。")

	if !got.Passed {
		t.Errorf("中性研究用語不該被黑名單誤殺: %s", got.Detail)
	}
}
