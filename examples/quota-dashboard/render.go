package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// 四档平色，彼此拉开。墨水屏上相邻灰阶几乎分不出来，
// 所以用量高低主要靠数字与条长表达，灰度只做强调。
var (
	ink     = color.Gray{Y: 0x00}
	muted   = color.Gray{Y: 0x6A}
	rule    = color.Gray{Y: 0xC8}
	track   = color.Gray{Y: 0xEA}
	surface = color.Gray{Y: 0xFF}
)

const margin = 48

type renderer struct {
	img  *image.Gray
	font *opentype.Font
	faces map[int]font.Face
}

func newRenderer(width, height int, fontData []byte) (*renderer, error) {
	f, err := opentype.Parse(fontData)
	if err != nil {
		return nil, fmt.Errorf("解析字体失败: %w", err)
	}
	img := image.NewGray(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{surface}, image.Point{}, draw.Src)
	return &renderer{img: img, font: f, faces: map[int]font.Face{}}, nil
}

func (r *renderer) face(size int) font.Face {
	if f, ok := r.faces[size]; ok {
		return f
	}
	f, err := opentype.NewFace(r.font, &opentype.FaceOptions{
		Size: float64(size), DPI: 72, Hinting: font.HintingFull,
	})
	if err != nil {
		panic(err)
	}
	r.faces[size] = f
	return f
}

func (r *renderer) textWidth(s string, size int) int {
	return font.MeasureString(r.face(size), s).Ceil()
}

func (r *renderer) text(x, y int, s string, size int, c color.Gray) {
	d := &font.Drawer{
		Dst:  r.img,
		Src:  &image.Uniform{c},
		Face: r.face(size),
		Dot:  fixed.P(x, y),
	}
	d.DrawString(s)
}

func (r *renderer) hline(x0, x1, y int, c color.Gray, thickness int) {
	for dy := 0; dy < thickness; dy++ {
		for x := x0; x < x1; x++ {
			r.img.SetGray(x, y+dy, c)
		}
	}
}

func (r *renderer) rect(x0, y0, x1, y1 int, c color.Gray) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			r.img.SetGray(x, y, c)
		}
	}
}

// tailOf 生成一行进度条之后的数值文字，image 与 text 两种模式共用：
// 「已用 N% · 已用/总额 · 重置倒计时」；被长周期覆盖的行只标限制来源，
// 不再展示自身原始计数（否则条上是生效值、旁边是原始值，对不上）。
func tailOf(w Window, now time.Time) string {
	var tail string
	switch {
	case w.Note != "":
		tail = w.Note
	case w.UsedPercent >= 0:
		tail = fmt.Sprintf("已用 %.0f%%", w.UsedPercent)
	default:
		tail = "—"
	}
	if w.ConstrainedBy != "" {
		tail += "（受" + w.ConstrainedBy + "额度限制）"
		return tail
	}
	if w.Total > 0 {
		tail += fmt.Sprintf(" · %s/%s", formatAmount(w.Used), formatAmount(w.Total))
	}
	if rt := resetText(w.ResetAt, now); rt != "" {
		tail += " · " + rt
	}
	return tail
}

// quotaBar 画一行用量：标签列 + 定宽进度条 + 条后数值文字（tailOf）。
// 列宽全部固定，跨行逐像素对齐。
func (r *renderer) quotaBar(x0, baseY int, name string, win Window, now time.Time) {
	const (
		labelCol = 150 // 标签列宽（5小时/本周/本月/余额/3.6 Flash 都放得下）
		barW     = 420 // 进度条定宽
		barH     = 32
	)
	r.text(x0, baseY, name, 24, ink)

	barX0 := x0 + labelCol
	barY0 := baseY - 24
	r.rect(barX0, barY0, barX0+barW, barY0+barH, track)

	pct := win.UsedPercent
	if pct > 100 {
		pct = 100
	}
	if pct > 0 {
		fillW := int(float64(barW) * pct / 100)
		if fillW < 2 {
			fillW = 2
		}
		fill := muted
		if pct >= 90 {
			fill = ink
		}
		r.rect(barX0, barY0, barX0+fillW, barY0+barH, fill)
	}

	tail := tailOf(win, now)
	r.text(barX0+barW+20, baseY-1, tail, 22, ink)
}

// resetText 把重置时间渲染成「3h12m 后重置」或「08-08 重置」。
func resetText(t time.Time, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := t.Sub(now)
	if d < 0 {
		return "即将重置"
	}
	if d < 48*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if h > 0 {
			return fmt.Sprintf("%dh%02dm 后重置", h, m)
		}
		return fmt.Sprintf("%dm 后重置", m)
	}
	return t.Local().Format("01-02") + " 重置"
}

// formatAmount 给绝对量加千分位；整数不带小数点。
func formatAmount(n float64) string {
	if n < 0 {
		return "?"
	}
	s := fmt.Sprintf("%.0f", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// render 把各服务分块画到面板上：每块一个厂商，块内按周期
// 每行一条完整的已用/可用进度条。
func (r *renderer) render(services []ServiceUsage, now time.Time) {
	w := r.img.Bounds().Dx()
	h := r.img.Bounds().Dy()
	right := w - margin

	r.text(margin, margin+50, "AI 额度概览", 50, ink)
	stamp := now.Local().Format("2006-01-02 15:04")
	r.text(right-r.textWidth(stamp, 26), margin+44, stamp, 26, muted)
	r.hline(margin, right, margin+78, ink, 3)

	const (
		blockHead = 40 // 厂商名行高
		rowHeight = 60 // 每个窗口的进度条行高（绝对量已并入条内文字，无明细行）
		blockGap  = 12 // 厂商之间的间距（靠留白分组）
	)

	y := margin + 148
	for _, s := range services {
		r.text(margin, y, s.Name, 32, ink)
		if s.Plan != "" {
			r.text(right-r.textWidth(s.Plan, 24), y, s.Plan, 24, muted)
		}
		y += blockHead

		if s.Err != nil {
			r.text(margin+8, y-4, "查询失败: "+s.Err.Error(), 22, muted)
			y += rowHeight
		} else {
			for _, win := range s.Windows {
				r.quotaBar(margin+8, y, win.Label, win, now)
				y += rowHeight
			}
		}
		y += blockGap
	}

	footer := "角落连点 3 次退出 · EInkRelay"
	r.hline(margin, right, h-margin-40, rule, 2)
	r.text(margin, h-margin-10, footer, 24, muted)
}

func (r *renderer) encode(w io.Writer) error {
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	return enc.Encode(w, r.img)
}
