package main

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	gtext "github.com/yuin/goldmark/text"
	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

var (
	// ErrMarkdownRender is the opaque render failure behind the frozen 422
	// render_failed response.
	ErrMarkdownRender = errors.New("markdown render failed")
	// ErrMissingGlyph is returned when the loaded fonts cannot draw a rune that
	// the document actually contains. Failing here is the whole point: the
	// alternative is a page of tofu boxes that looks like a successful render.
	ErrMissingGlyph = errors.New("the loaded fonts have no glyph for a required rune")
)

// MarkdownStyle is the entire typographic configuration. Sizes are points at
// the library's DPI; margins, gaps and indents are pixels.
type MarkdownStyle struct {
	BaseSize     float64
	HeadingSizes [6]float64
	LineSpacing  float64
	ParagraphGap int
	Margin       int
	IndentStep   int
	QuoteBar     int
}

func DefaultMarkdownStyle() MarkdownStyle {
	return MarkdownStyle{
		BaseSize:     18,
		HeadingSizes: [6]float64{32, 27, 24, 21, 19, 18},
		LineSpacing:  1.35,
		ParagraphGap: 12,
		Margin:       28,
		IndentStep:   26,
		QuoteBar:     3,
	}
}

// minFontSize/maxFontSize bound the optional font_size request parameter. The
// floor keeps body text legible on an e-ink panel; the ceiling keeps a page of
// content meaningfully dense instead of a few giant glyphs.
const (
	minFontSize = 12
	maxFontSize = 72
)

// ScaledMarkdownStyle derives the full typographic configuration from a body
// size in points, keeping DefaultMarkdownStyle's proportions: point sizes scale
// exactly, pixel fields round to the nearest integer, and the line-spacing
// ratio carries over unchanged.
func ScaledMarkdownStyle(baseSize float64) MarkdownStyle {
	base := DefaultMarkdownStyle()
	factor := baseSize / base.BaseSize
	scaled := MarkdownStyle{
		BaseSize:    baseSize,
		LineSpacing: base.LineSpacing,
	}
	for index, size := range base.HeadingSizes {
		scaled.HeadingSizes[index] = size * factor
	}
	scale := func(pixels int) int { return int(float64(pixels)*factor + 0.5) }
	scaled.ParagraphGap = scale(base.ParagraphGap)
	scaled.Margin = scale(base.Margin)
	scaled.IndentStep = scale(base.IndentStep)
	scaled.QuoteBar = scale(base.QuoteBar)
	return scaled
}

type textStyle struct {
	role string
	size float64
}

type styledRun struct {
	text  string
	style textStyle
}

// glyphCell is one measured rune. Breaking decisions are made on cells rather
// than on strings so a Latin word and a run of Han characters can obey
// different rules without a second measuring pass.
type glyphCell struct {
	r       rune
	style   textStyle
	advance fixed.Int26_6
	forced  bool
}

type renderLine struct {
	runs      []styledRun
	indent    int
	gapBefore int
	quote     bool
	rule      bool
}

type markdownLayout struct {
	library *FontLibrary
	style   MarkdownStyle
	source  []byte
	width   int
	lines   []renderLine
	marker  *styledRun
	quote   bool
}

// RenderMarkdown turns CommonMark source into a full-screen grayscale PNG whose
// geometry matches the probed panel exactly.
//
// goldmark is constructed with no extensions, so this is CommonMark core only:
// no GFM tables, no autolink extension, no raw HTML rendering. Raw HTML blocks
// and inline HTML are dropped, links keep their label and lose their
// destination, and images degrade to their alt text. Nothing in this path opens
// a network connection.
func RenderMarkdown(source []byte, screen ScreenCapabilities, library *FontLibrary, style MarkdownStyle) ([]byte, error) {
	if screen.Width < 1 || screen.Height < 1 {
		return nil, ErrMarkdownRender
	}
	if library == nil {
		return nil, ErrFontMissing
	}
	layout := &markdownLayout{
		library: library,
		style:   style,
		source:  source,
		width:   screen.Width - 2*style.Margin,
	}
	if layout.width < 1 || style.BaseSize <= 0 {
		return nil, ErrMarkdownRender
	}
	document := goldmark.New().Parser().Parse(gtext.NewReader(source))
	if err := layout.block(document, 0); err != nil {
		return nil, err
	}
	canvas, err := layout.draw(screen)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	if err := encodeScreenPNG(&buffer, canvas); err != nil {
		return nil, ErrMarkdownRender
	}
	return buffer.Bytes(), nil
}

func (l *markdownLayout) block(node ast.Node, indent int) error {
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		if err := l.node(child, indent); err != nil {
			return err
		}
	}
	return nil
}

// node dispatches on the block kind. Anything unrecognised degrades to its
// children rather than being dropped, which is what keeps an unknown construct
// readable instead of invisible.
func (l *markdownLayout) node(node ast.Node, indent int) error {
	switch node.Kind() {
	case ast.KindHeading:
		heading := node.(*ast.Heading)
		size := l.style.BaseSize
		if heading.Level >= 1 && heading.Level <= 6 {
			size = l.style.HeadingSizes[heading.Level-1]
		}
		runs, err := l.inline(node, textStyle{role: fontRoleBold, size: size})
		if err != nil {
			return err
		}
		return l.emit(runs, indent, l.style.ParagraphGap)
	case ast.KindParagraph:
		runs, err := l.inline(node, textStyle{role: fontRoleRegular, size: l.style.BaseSize})
		if err != nil {
			return err
		}
		return l.emit(runs, indent, l.style.ParagraphGap)
	case ast.KindTextBlock:
		// A tight list item's content: no paragraph gap, otherwise identical.
		runs, err := l.inline(node, textStyle{role: fontRoleRegular, size: l.style.BaseSize})
		if err != nil {
			return err
		}
		return l.emit(runs, indent, 0)
	case ast.KindBlockquote:
		previous := l.quote
		l.quote = true
		err := l.block(node, indent+l.style.IndentStep)
		l.quote = previous
		return err
	case ast.KindList:
		return l.list(node.(*ast.List), indent)
	case ast.KindFencedCodeBlock:
		return l.code(node.(*ast.FencedCodeBlock).Lines(), indent)
	case ast.KindCodeBlock:
		return l.code(node.(*ast.CodeBlock).Lines(), indent)
	case ast.KindThematicBreak:
		// Drawn as a rule rather than as glyphs, so it cannot depend on a
		// character the bundled fonts might not carry.
		l.lines = append(l.lines, renderLine{indent: indent, gapBefore: l.style.ParagraphGap, rule: true})
		return nil
	case ast.KindHTMLBlock:
		// Raw HTML is never executed and never drawn.
		return nil
	default:
		return l.block(node, indent)
	}
}

func (l *markdownLayout) list(list *ast.List, indent int) error {
	number := list.Start
	if number < 1 {
		number = 1
	}
	style := textStyle{role: fontRoleRegular, size: l.style.BaseSize}
	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		marker := "• "
		if list.IsOrdered() {
			marker = strconv.Itoa(number) + ". "
			number++
		}
		l.marker = &styledRun{text: marker, style: style}
		if err := l.block(item, indent+l.style.IndentStep); err != nil {
			return err
		}
		l.marker = nil
	}
	return nil
}

// code lays out a code block verbatim: one source line per layout line, in the
// monospace role, with no inline parsing at all.
func (l *markdownLayout) code(lines *gtext.Segments, indent int) error {
	if lines == nil {
		return nil
	}
	style := textStyle{role: fontRoleMono, size: l.style.BaseSize * 0.9}
	gap := l.style.ParagraphGap
	for index := 0; index < lines.Len(); index++ {
		segment := lines.At(index)
		content := strings.TrimRight(string(segment.Value(l.source)), "\r\n")
		if err := l.emit([]styledRun{{text: content, style: style}}, indent+l.style.IndentStep, gap); err != nil {
			return err
		}
		gap = 0
	}
	return nil
}

func (l *markdownLayout) inline(node ast.Node, base textStyle) ([]styledRun, error) {
	runs := make([]styledRun, 0, 8)
	if err := l.appendInline(&runs, node, base); err != nil {
		return nil, err
	}
	return runs, nil
}

func (l *markdownLayout) appendInline(runs *[]styledRun, node ast.Node, style textStyle) error {
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		switch child.Kind() {
		case ast.KindText:
			typed := child.(*ast.Text)
			*runs = append(*runs, styledRun{text: string(typed.Segment.Value(l.source)), style: style})
			if typed.HardLineBreak() {
				*runs = append(*runs, styledRun{text: "\n", style: style})
			} else if typed.SoftLineBreak() {
				*runs = append(*runs, styledRun{text: " ", style: style})
			}
		case ast.KindString:
			*runs = append(*runs, styledRun{text: string(child.(*ast.String).Value), style: style})
		case ast.KindEmphasis:
			next := style
			if child.(*ast.Emphasis).Level >= 2 || style.role == fontRoleBold {
				// Emphasis inside an already-bold heading stays bold rather
				// than dropping back to a lighter italic face.
				next.role = fontRoleBold
			} else {
				next.role = fontRoleItalic
			}
			if err := l.appendInline(runs, child, next); err != nil {
				return err
			}
		case ast.KindCodeSpan:
			next := style
			next.role = fontRoleMono
			if err := l.appendInline(runs, child, next); err != nil {
				return err
			}
		case ast.KindAutoLink:
			*runs = append(*runs, styledRun{text: string(child.(*ast.AutoLink).Label(l.source)), style: style})
		case ast.KindLink, ast.KindImage:
			// The label (or alt text) is kept; the destination is never
			// resolved, opened or fetched.
			if err := l.appendInline(runs, child, style); err != nil {
				return err
			}
		case ast.KindRawHTML:
			// Inline HTML is dropped, never executed.
		default:
			if err := l.appendInline(runs, child, style); err != nil {
				return err
			}
		}
	}
	return nil
}

func (l *markdownLayout) emit(runs []styledRun, indent, gapBefore int) error {
	if l.marker != nil {
		runs = append([]styledRun{*l.marker}, runs...)
		l.marker = nil
	}
	wrapped, err := l.wrap(runs, indent)
	if err != nil {
		return err
	}
	for index, line := range wrapped {
		gap := 0
		if index == 0 {
			gap = gapBefore
		}
		l.lines = append(l.lines, renderLine{runs: line, indent: indent, gapBefore: gap, quote: l.quote})
	}
	return nil
}

// wrap measures every rune and breaks greedily. All arithmetic is in 26.6 fixed
// point, so the same source produces the same line breaks on darwin/arm64 and
// on linux/arm.
func (l *markdownLayout) wrap(runs []styledRun, indent int) ([][]styledRun, error) {
	limit := l.width - indent
	if limit < 1 {
		limit = 1
	}
	limitFixed := fixed.I(limit)

	cells := make([]glyphCell, 0, 64)
	for _, run := range runs {
		face, err := l.library.Face(run.style.role, run.style.size)
		if err != nil {
			return nil, err
		}
		for _, r := range run.text {
			if r == '\n' {
				cells = append(cells, glyphCell{r: r, style: run.style, forced: true})
				continue
			}
			if r == '\r' || r == '\t' {
				r = ' '
			}
			advance, ok := face.GlyphAdvance(r)
			if !ok {
				// Fail closed rather than draw a notdef box: a document the
				// bundled fonts cannot render is an error, not a screen.
				return nil, ErrMissingGlyph
			}
			cells = append(cells, glyphCell{r: r, style: run.style, advance: advance})
		}
	}

	var out [][]styledRun
	line := make([]glyphCell, 0, 64)
	var width fixed.Int26_6
	breakAt := -1
	for index := 0; index < len(cells); index++ {
		current := cells[index]
		if current.forced {
			out = append(out, cellsToRuns(trimTrailingSpaceCells(line)))
			line = line[:0]
			width = 0
			breakAt = -1
			continue
		}
		if len(line) > 0 && width+current.advance > limitFixed {
			cut := len(line)
			if breakAt > 0 {
				cut = breakAt
			}
			taken := append([]glyphCell{}, line[:cut]...)
			out = append(out, cellsToRuns(trimTrailingSpaceCells(taken)))
			rest := append([]glyphCell{}, line[cut:]...)
			for len(rest) > 0 && rest[0].r == ' ' {
				rest = rest[1:]
			}
			line = rest
			width = 0
			for _, cell := range line {
				width += cell.advance
			}
			breakAt = -1
		}
		line = append(line, current)
		width += current.advance
		if allowBreakAfter(cells, index) {
			breakAt = len(line)
		}
	}
	if len(line) > 0 || len(out) == 0 {
		out = append(out, cellsToRuns(trimTrailingSpaceCells(line)))
	}
	return out, nil
}

func cellsToRuns(cells []glyphCell) []styledRun {
	runs := make([]styledRun, 0, 4)
	var builder strings.Builder
	var current textStyle
	for index, cell := range cells {
		if index == 0 {
			current = cell.style
		} else if cell.style != current {
			runs = append(runs, styledRun{text: builder.String(), style: current})
			builder.Reset()
			current = cell.style
		}
		builder.WriteRune(cell.r)
	}
	if builder.Len() > 0 {
		runs = append(runs, styledRun{text: builder.String(), style: current})
	}
	return runs
}

func trimTrailingSpaceCells(cells []glyphCell) []glyphCell {
	end := len(cells)
	for end > 0 && cells[end-1].r == ' ' {
		end--
	}
	return cells[:end]
}

// isCJK covers the scripts that break between characters rather than between
// words. It is deliberately generous: a false positive only adds a legal break
// opportunity, whereas a false negative produces an overlong line.
func isCJK(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x11ff:
		return true
	case r >= 0x2e80 && r <= 0x303e:
		return true
	case r >= 0x3041 && r <= 0x33ff:
		return true
	case r >= 0x3400 && r <= 0x4dbf:
		return true
	case r >= 0x4e00 && r <= 0x9fff:
		return true
	case r >= 0xa000 && r <= 0xa4cf:
		return true
	case r >= 0xac00 && r <= 0xd7a3:
		return true
	case r >= 0xf900 && r <= 0xfaff:
		return true
	case r >= 0xfe30 && r <= 0xfe4f:
		return true
	case r >= 0xff00 && r <= 0xff60:
		return true
	case r >= 0xffe0 && r <= 0xffe6:
		return true
	case r >= 0x20000 && r <= 0x3ffff:
		return true
	}
	return false
}

// The minimal kinsoku rules: a closing mark never starts a line and an opening
// mark never ends one.
const (
	cjkClosing = "、。，．！？：；）〕］｝〉》」』】’”"
	cjkOpening = "（〔［｛〈《「『【‘“"
)

func allowBreakAfter(cells []glyphCell, index int) bool {
	if index+1 >= len(cells) {
		return false
	}
	current := cells[index].r
	next := cells[index+1].r
	if current == ' ' {
		return next != ' '
	}
	if next == ' ' {
		return false
	}
	if strings.ContainsRune(cjkOpening, current) || strings.ContainsRune(cjkClosing, next) {
		return false
	}
	return isCJK(current) || isCJK(next)
}

func (l *markdownLayout) metrics(line renderLine) (int, int, error) {
	if len(line.runs) == 0 {
		face, err := l.library.Face(fontRoleRegular, l.style.BaseSize)
		if err != nil {
			return 0, 0, err
		}
		measured := face.Metrics()
		return measured.Ascent.Ceil(), measured.Descent.Ceil(), nil
	}
	ascent, descent := 0, 0
	for _, run := range line.runs {
		face, err := l.library.Face(run.style.role, run.style.size)
		if err != nil {
			return 0, 0, err
		}
		measured := face.Metrics()
		if value := measured.Ascent.Ceil(); value > ascent {
			ascent = value
		}
		if value := measured.Descent.Ceil(); value > descent {
			descent = value
		}
	}
	return ascent, descent, nil
}

func (l *markdownLayout) draw(screen ScreenCapabilities) (*image.Gray, error) {
	canvas := image.NewGray(image.Rect(0, 0, screen.Width, screen.Height))
	for index := range canvas.Pix {
		canvas.Pix[index] = 0xff
	}
	ink := image.NewUniform(color.Gray{Y: 0x00})
	y := l.style.Margin
	bottom := screen.Height - l.style.Margin
	for _, line := range l.lines {
		y += line.gapBefore
		if line.rule {
			if y+2 > bottom {
				break
			}
			l.paintRect(canvas, l.style.Margin+line.indent, y, screen.Width-2*l.style.Margin-line.indent, 1, 0x60)
			y += 2 + l.style.ParagraphGap
			continue
		}
		ascent, descent, err := l.metrics(line)
		if err != nil {
			return nil, err
		}
		if y+ascent+descent > bottom {
			// The panel is full. Truncating is the documented behaviour; there
			// is no pagination in v0.1.
			break
		}
		if line.quote {
			l.paintRect(canvas, l.style.Margin+line.indent-l.style.IndentStep/2, y, l.style.QuoteBar, ascent+descent, 0x40)
		}
		dot := fixed.P(l.style.Margin+line.indent, y+ascent)
		for _, run := range line.runs {
			face, err := l.library.Face(run.style.role, run.style.size)
			if err != nil {
				return nil, err
			}
			drawer := &font.Drawer{Dst: canvas, Src: ink, Face: face, Dot: dot}
			drawer.DrawString(run.text)
			dot = drawer.Dot
		}
		y += ascent + descent
		if extra := int(float64(ascent+descent) * (l.style.LineSpacing - 1)); extra > 0 {
			y += extra
		}
	}
	return canvas, nil
}

func (l *markdownLayout) paintRect(canvas *image.Gray, left, top, width, height int, shade uint8) {
	for y := top; y < top+height; y++ {
		if y < canvas.Rect.Min.Y || y >= canvas.Rect.Max.Y {
			continue
		}
		for x := left; x < left+width; x++ {
			if x < canvas.Rect.Min.X || x >= canvas.Rect.Max.X {
				continue
			}
			canvas.Pix[y*canvas.Stride+x] = shade
		}
	}
}
