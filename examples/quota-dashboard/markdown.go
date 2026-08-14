package main

import (
	"fmt"
	"strings"
	"time"
)

// 文本（Markdown）模式：排版在设备端完成，主机只发一段 Markdown。
// 约束（真机实测）：无 GFM 表格、粗体/等宽回退比例字体、跨行空格对齐不可靠——
// 所以进度条用块字符 █/░（同名字宽一致，同行内不涉及跨行对齐），每格 5%。
// 20pt 下 9 家厂商恰好排满 1072x1448，超出部分设备端按行截断。

const textFontSize = 20

// textBar 渲染 20 格块字符进度条，每格 5%。
func textBar(pct float64) string {
	if pct < 0 {
		pct = 0
	}
	filled := int(pct/5 + 0.5)
	if filled > 20 {
		filled = 20
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", 20-filled)
}

// toFullwidth 把 ASCII 可打印字符转成等宽全角形式（空格转 U+3000）。
// 钉死的 Noto Sans CJK 里全角字符与块字符都是整 1em 宽，
// 标签列因此可以按 em 精确对齐——与比例字体无关，也不靠会被
// Markdown 折叠的 ASCII 空格或制表符。
func toFullwidth(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ':
			b.WriteRune('　')
		case r >= '!' && r <= '~':
			b.WriteRune(r + 0xFEE0)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// emLen 以全角字符为单位计宽（每个全角字符 1em）。
func emLen(s string) int { return len([]rune(s)) }

func planOf(name string, services []ServiceUsage) string {
	for _, s := range services {
		if s.Name == name {
			return s.Plan
		}
	}
	return ""
}

// buildMarkdown 把查询结果排版成设备端 Markdown。
func buildMarkdown(services []ServiceUsage, now time.Time) string {
	type row struct {
		service string
		label   string
		tail    string
		pct     float64
	}
	var rows []row
	maxEm := 0
	for _, s := range services {
		if s.Err != nil {
			rows = append(rows, row{service: s.Name, label: "", tail: "查询失败：" + s.Err.Error(), pct: -1})
			continue
		}
		for _, w := range s.Windows {
			label := toFullwidth(w.Label)
			tail := tailOf(w, now)
			rows = append(rows, row{service: s.Name, label: label, tail: tail, pct: w.UsedPercent})
			if n := emLen(label); n > maxEm {
				maxEm = n
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# AI 额度概览 · %s\n", now.Local().Format("15:04"))
	lastService := ""
	for _, r := range rows {
		if r.service != lastService {
			plan := planOf(r.service, services)
			if plan != "" {
				fmt.Fprintf(&b, "\n## %s · %s\n", r.service, plan)
			} else {
				fmt.Fprintf(&b, "\n## %s\n", r.service)
			}
			lastService = r.service
		}
		if r.pct < 0 {
			fmt.Fprintf(&b, "\n%s\n", r.tail)
			continue
		}
		// 标签列补齐到统一 em 宽（再留 1em 间隙）+ 固定 20 格进度条：
		// 条起点、条后文字起点因此逐行像素级对齐。
		pad := strings.Repeat("　", maxEm+1-emLen(r.label))
		fmt.Fprintf(&b, "\n%s%s%s %s\n", r.label, pad, textBar(r.pct), r.tail)
	}
	return b.String()
}
