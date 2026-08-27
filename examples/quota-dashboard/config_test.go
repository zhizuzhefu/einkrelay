package main

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func enabledIDs(t *testing.T, spec string) string {
	t.Helper()
	ps, err := resolveProviders(spec)
	if err != nil {
		t.Fatalf("resolveProviders(%q) 意外失败: %v", spec, err)
	}
	return strings.Join(idsOf(ps), ",")
}

func idsOf(ps []provider) []string {
	ids := make([]string, len(ps))
	for i, p := range ps {
		ids[i] = p.ID
	}
	return ids
}

// allExcept 按默认顺序列出除给定 ID 外的全部来源。
func allExcept(drop ...string) string {
	var keep []string
	for _, id := range providerIDs() {
		if !slicesContains(drop, id) {
			keep = append(keep, id)
		}
	}
	return strings.Join(keep, ",")
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestResolveProviders(t *testing.T) {
	all := allExcept()
	cases := []struct{ spec, want string }{
		{"", all},                        // 空配置 = 全部（保持旧行为）
		{"   ", all},                     //
		{"all", all},                     //
		{"CLAUDE, GLM ", "claude,glm"},   // 大小写与空格不敏感
		{"glm,claude", "glm,claude"},     // 配置顺序即展示顺序
		{"claude,claude", "claude"},      // 重复只算一次
		{"all,-grok", allExcept("grok")}, // 全部但排除
		{"all,!grok,!codex", allExcept("grok", "codex")}, // ! 与 - 同义
		{"all,-grok,grok", allExcept("grok") + ",grok"},  // 排除后可再加回队尾
		{"claude,-deepseek", "claude"},                   // 排除未启用的来源是空操作
		{"-all,claude", "claude"},                        // -all 先清空
		{" deepseek , minimax ", "deepseek,minimax"},
	}
	for _, tc := range cases {
		if got := enabledIDs(t, tc.spec); got != tc.want {
			t.Errorf("resolveProviders(%q)=%q want %q", tc.spec, got, tc.want)
		}
	}
}

func TestResolveProvidersErrors(t *testing.T) {
	// 未知 ID 与「一家都没启用」都必须报错，不能静默渲染一块空面板。
	for _, spec := range []string{"claude,gemini", "-all", "all,-all", ",,", "claude,-claude"} {
		if ps, err := resolveProviders(spec); err == nil {
			t.Errorf("resolveProviders(%q) 应当报错，却返回了 %v", spec, idsOf(ps))
		}
	}
}

// 未启用的来源不应被查询：probeAll 只调用配置里的 Probe，
// 且返回顺序与配置一致。
func TestProbeAllRespectsConfig(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	stub := func(id string) provider {
		return provider{ID: id, Name: id, Probe: func(ctx context.Context) ServiceUsage {
			mu.Lock()
			calls[id]++
			mu.Unlock()
			return ServiceUsage{Name: id}
		}}
	}

	got := probeAll(context.Background(), []provider{stub("b"), stub("a")})

	if len(got) != 2 || got[0].Name != "b" || got[1].Name != "a" {
		t.Fatalf("probeAll 返回顺序不对: %v", got)
	}
	if len(calls) != 2 || calls["b"] != 1 || calls["a"] != 1 {
		t.Fatalf("探测调用次数不对: %v", calls)
	}
}
