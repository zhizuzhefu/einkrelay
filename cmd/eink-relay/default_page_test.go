package main

import (
	"testing"

	"github.com/yuin/goldmark"
	gtext "github.com/yuin/goldmark/text"
)

// TestDefaultHelpPageFitsPW4WithoutTruncation mirrors the draw loop's vertical
// accounting: the help page is instructional text that only helps if it is
// complete, and the v0.1 renderer truncates silently when the panel is full,
// so the page must fit the PW4 geometry (1072x1448) at its enlarged style.
func TestDefaultHelpPageFitsPW4WithoutTruncation(t *testing.T) {
	library := installedCJKLibrary(t)
	style := defaultHelpPageStyle()
	layout := &markdownLayout{
		library: library,
		style:   style,
		source:  []byte(defaultHelpPageMarkdown),
		width:   1072 - 2*style.Margin,
	}
	document := goldmark.New().Parser().Parse(gtext.NewReader(layout.source))
	if err := layout.block(document, 0); err != nil {
		t.Fatal(err)
	}
	y := style.Margin
	bottom := 1448 - style.Margin
	for _, line := range layout.lines {
		y += line.gapBefore
		if line.rule {
			y += 2 + style.ParagraphGap
			continue
		}
		ascent, descent, err := layout.metrics(line)
		if err != nil {
			t.Fatal(err)
		}
		if y+ascent+descent > bottom {
			t.Fatalf("the help page overflows the PW4 panel at y=%d needing %d with bottom %d", y, ascent+descent, bottom)
		}
		y += ascent + descent
		if extra := int(float64(ascent+descent) * (style.LineSpacing - 1)); extra > 0 {
			y += extra
		}
	}
}
