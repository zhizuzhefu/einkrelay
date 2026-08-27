package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	out := flag.String("o", "", "只把渲染结果写到该文件，不推送到设备（image 模式写 PNG，text 模式写 Markdown）")
	fontPath := flag.String("font", os.Getenv("DASHBOARD_FONT"), "CJK 字体文件路径（或用 DASHBOARD_FONT）；text 模式不需要")
	format := flag.String("format", "image", "显示格式：image（主机渲染 PNG，推荐）或 text（设备端排版 Markdown）")
	every := flag.Duration("every", 0, "每隔多久自动刷新一次（如 5m）；0 表示只跑一次")
	providersSpec := flag.String("providers", os.Getenv("DASHBOARD_PROVIDERS"),
		"启用哪些额度来源（或用 DASHBOARD_PROVIDERS）：逗号分隔的 "+
			strings.Join(providerIDs(), "/")+"，顺序即展示顺序；"+
			"默认 all（全部），all,-grok 表示全部但排除 Grok。只有启用的来源才会被查询和展示")
	flag.Parse()

	if *format != "image" && *format != "text" {
		fmt.Fprintln(os.Stderr, "-format 只能是 image 或 text")
		os.Exit(1)
	}
	providers, err := resolveProviders(*providersSpec)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var fontData []byte
	if *format == "image" {
		if *fontPath == "" {
			fmt.Fprintln(os.Stderr, "需要 CJK 字体：设置 DASHBOARD_FONT 或 -font（固定字体见 assets/fonts/manifest.json 的 URL）")
			os.Exit(1)
		}
		fontData, err = os.ReadFile(*fontPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "读取字体失败:", err)
			os.Exit(1)
		}
	}

	host := os.Getenv("EINKRELAY_HOST")
	token := os.Getenv("EINKRELAY_TOKEN")

	// 面板几何只在 image 模式启动时问一次；text 模式由设备端排版，无需询问。
	width, height := 1072, 1448 // PW4；能问到设备就以设备为准
	if *out == "" {
		if host == "" || token == "" {
			fmt.Fprintln(os.Stderr, "推送模式需要 EINKRELAY_HOST 与 EINKRELAY_TOKEN；或加 -o 只写文件")
			os.Exit(1)
		}
		if *format == "image" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			w, h, err := panelSize(ctx, host, token)
			cancel()
			if err != nil {
				fmt.Fprintln(os.Stderr, "查询面板几何失败:", err)
				os.Exit(1)
			}
			width, height = w, h
		}
	}

	for {
		err := runOnce(providers, fontData, width, height, *out, *format, host, token)
		if *every <= 0 {
			if err != nil {
				os.Exit(1)
			}
			return
		}
		if err != nil {
			// 循环模式下单次失败（设备暂时离线、409、某家接口抖动）
			// 只记录，不退出——下一个周期再试。
			fmt.Fprintln(os.Stderr, "本轮失败:", err)
		}
		time.Sleep(*every)
	}
}

func runOnce(providers []provider, fontData []byte, width, height int, out, format, host, token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	start := time.Now()
	services := probeAll(ctx, providers)
	fmt.Fprintf(os.Stderr, "额度查询完成，耗时 %s\n", time.Since(start).Round(time.Millisecond))
	for _, s := range services {
		if s.Err != nil {
			fmt.Fprintf(os.Stderr, "  %-11s 查询失败: %v\n", s.Name, s.Err)
			continue
		}
		for _, w := range s.Windows {
			line := fmt.Sprintf("  %-11s %-20s", s.Name, w.Label)
			if w.Note == "" && w.UsedPercent >= 0 {
				line += fmt.Sprintf(" 已用 %5.1f%%", w.UsedPercent)
			}
			if w.Total > 0 {
				line += fmt.Sprintf("  总额 %s · 可用 %s · 已用 %s",
					formatAmount(w.Total), formatAmount(w.Total-w.Used), formatAmount(w.Used))
			}
			if w.Note != "" {
				line += "  " + w.Note
			}
			if !w.ResetAt.IsZero() {
				line += "  重置 " + w.ResetAt.Local().Format("01-02 15:04")
			}
			if w.ConstrainedBy != "" {
				line += "（受" + w.ConstrainedBy + "额度限制）"
			}
			fmt.Fprintln(os.Stderr, line)
		}
	}

	if format == "text" {
		body := buildMarkdown(services, time.Now())
		if out != "" {
			if err := os.WriteFile(out, []byte(body), 0o644); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "已写入", out)
			return nil
		}
		pushStart := time.Now()
		if err := pushMarkdown(ctx, host, token, body); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "已推送 %d 字节 Markdown，推送+显示耗时 %s\n",
			len(body), time.Since(pushStart).Round(time.Millisecond))
		return nil
	}

	r, err := newRenderer(width, height, fontData)
	if err != nil {
		return err
	}
	r.render(services, time.Now())

	if out != "" {
		f, err := os.Create(out)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := r.encode(f); err != nil {
			return fmt.Errorf("编码 PNG 失败: %w", err)
		}
		fmt.Fprintln(os.Stderr, "已写入", out)
		return nil
	}

	f, err := os.CreateTemp("", "quota-dashboard-*.png")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := r.encode(f); err != nil {
		return fmt.Errorf("编码 PNG 失败: %w", err)
	}
	f.Close()
	payload, err := os.ReadFile(f.Name())
	if err != nil {
		return err
	}

	pushStart := time.Now()
	if err := push(ctx, host, token, payload); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "已推送 %d KiB 到 %dx%d 面板，推送+显示耗时 %s\n",
		len(payload)/1024, width, height, time.Since(pushStart).Round(time.Millisecond))
	return nil
}
