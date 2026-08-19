// Package eval 是 LLM 輸出忠實度驗證：golden set 跑過 report 完整管線，
// 斷言「該提的有提、方向沒講反、建議性字眼零出現」。
// 改 prompt / 換 provider 必重跑（docs/phase-1.md §3）。
package eval

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/brucechen520/daily-stock/internal/llm"
	"github.com/brucechen520/daily-stock/internal/report"
	"gopkg.in/yaml.v3"
)

//go:embed cases.yaml
var casesYAML []byte

// Blacklist 是全案通用的建議性字眼黑名單——任何摘要出現即 fail（M7 紀律）。
var Blacklist = []string{
	"建議買進", "建議賣出", "建議進場", "建議出場",
	"應該買", "應該賣", "可以買進", "可以賣出",
	"進場佈局", "逢低買進", "獲利了結", "停損出場",
	"加碼", "減碼", "目標價", "上看", "下看",
}

type Case struct {
	Name        string   `yaml:"name"`
	IndexClose  float64  `yaml:"index_close"`
	IndexChange float64  `yaml:"index_change"`
	ForeignNet  int64    `yaml:"foreign_net"`
	TrustNet    int64    `yaml:"trust_net"`
	DealerNet   int64    `yaml:"dealer_net"`
	MustMention []string `yaml:"must_mention"`
	Forbidden   []string `yaml:"forbidden"`
}

func LoadCases() ([]Case, error) {
	var cases []Case
	if err := yaml.Unmarshal(casesYAML, &cases); err != nil {
		return nil, fmt.Errorf("eval: %w", err)
	}
	return cases, nil
}

func (c Case) snapshot() report.Snapshot {
	return report.Snapshot{
		Date:        time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC), // 固定日期，教學術語輪替可重現
		IndexClose:  c.IndexClose,
		IndexChange: c.IndexChange,
		ForeignNet:  c.ForeignNet,
		TrustNet:    c.TrustNet,
		DealerNet:   c.DealerNet,
	}
}

type CaseResult struct {
	Name   string
	Passed bool
	Detail string // fail 原因；pass 為空
}

type Result struct {
	Provider string
	Model    string
	Cases    []CaseResult
}

func (r Result) Passed() int {
	n := 0
	for _, c := range r.Cases {
		if c.Passed {
			n++
		}
	}
	return n
}

// Check 對「代入後的最終輸出」跑斷言。純函式，單測直接打。
func Check(c Case, output string) CaseResult {
	var fails []string
	for _, m := range c.MustMention {
		if !strings.Contains(output, m) {
			fails = append(fails, fmt.Sprintf("缺少 %q", m))
		}
	}
	for _, f := range append(append([]string{}, c.Forbidden...), Blacklist...) {
		if strings.Contains(output, f) {
			fails = append(fails, fmt.Sprintf("出現禁詞 %q", f))
		}
	}
	// golden set 每組資料都齊全，聲稱缺資料即為誤用（Validate 攔得掉大半，
	// 這裡再攔一次是因為 Validate 只看代入前的原文，教學段與模板不經過它）。
	if strings.Contains(output, report.NoDataPhrase) {
		fails = append(fails, fmt.Sprintf("出現 %q，但本案資料齊全", report.NoDataPhrase))
	}
	if len(fails) > 0 {
		return CaseResult{Name: c.Name, Passed: false, Detail: strings.Join(fails, "; ")}
	}
	return CaseResult{Name: c.Name, Passed: true}
}

// Run 跑整組 golden set（每 case 走一次完整 report 管線 = 真的打 LLM）。
func Run(ctx context.Context, p llm.Provider) (*Result, error) {
	cases, err := LoadCases()
	if err != nil {
		return nil, err
	}
	provider, model := p.Name()
	res := &Result{Provider: provider, Model: model}
	for _, c := range cases {
		out, err := report.Generate(ctx, p, c.snapshot())
		if err != nil {
			res.Cases = append(res.Cases, CaseResult{
				Name: c.Name, Passed: false, Detail: fmt.Sprintf("生成失敗: %v", err),
			})
			continue
		}
		res.Cases = append(res.Cases, Check(c, out))
	}
	return res, nil
}
