package main

import "testing"

func TestVersionSegments(t *testing.T) {
	cases := []struct {
		name string
		want []int
	}{
		{"Gemini 3.6 Flash", []int{3, 6}},
		{"gemini-3.10-flash", []int{3, 10}},
		{"Gemini 3 Flash", []int{3}},
		{"GPT-OSS 120B (Medium)", []int{120}},
		{"chat_20706", []int{20706}}, // 任意数字都会被提取；这类族靠 gemini 关键词过滤排除
		{"chat", nil},
	}
	for _, c := range cases {
		got := versionSegments(c.name)
		if len(got) != len(c.want) {
			t.Errorf("versionSegments(%q) = %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("versionSegments(%q) = %v, want %v", c.name, got, c.want)
				break
			}
		}
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b []int
		want int // >0 表示 a 更新
	}{
		{[]int{3, 10}, []int{3, 9}, 1},  // 3.10 > 3.9，ParseFloat 会算反
		{[]int{3, 6}, []int{3, 7}, -1},  //
		{[]int{4}, []int{3, 9}, 1},      // 大版本优先
		{[]int{3, 6, 1}, []int{3, 6}, 1}, // 前缀相同段数多的更新
		{[]int{3, 6}, []int{3, 6}, 0},
	}
	for _, c := range cases {
		got := compareVersions(c.a, c.b)
		if (got > 0) != (c.want > 0) || (got == 0) != (c.want == 0) {
			t.Errorf("compareVersions(%v, %v) = %d, want 符号 %d", c.a, c.b, got, c.want)
		}
	}
}
