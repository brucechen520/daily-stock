// Package glossary 是投資術語辭典。定義是 deterministic 的（YAML 為準），
// 【術語小教室】由程式直接引用，LLM 不得自行發明或改寫定義。
package glossary

import (
	_ "embed"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed terms.yaml
var termsYAML []byte

type Term struct {
	Term        string   `yaml:"term"`
	Keywords    []string `yaml:"keywords"`
	Explanation string   `yaml:"explanation"`
}

// Load 解析內嵌辭典。
func Load() ([]Term, error) {
	var terms []Term
	if err := yaml.Unmarshal(termsYAML, &terms); err != nil {
		return nil, fmt.Errorf("glossary: %w", err)
	}
	if len(terms) == 0 {
		return nil, fmt.Errorf("glossary: 辭典是空的")
	}
	return terms, nil
}

// PickRelevant 從「當日摘要素材文字」挑一個相關術語做教學。
// seed（用日期天數）做輪替——同一天固定、不同天輪流，教學不重複又可重現。
func PickRelevant(terms []Term, contextText string, seed int) *Term {
	var hits []Term
	for _, t := range terms {
		for _, kw := range t.Keywords {
			if strings.Contains(contextText, kw) {
				hits = append(hits, t)
				break
			}
		}
	}
	if len(hits) == 0 {
		hits = terms // 都不相關就全部輪替（至少教一個）
	}
	if seed < 0 {
		seed = -seed
	}
	return &hits[seed%len(hits)]
}
