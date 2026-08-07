package main

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"testing"

	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goitalic"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/goregular"
)

// latinLibrary is built from the Go fonts that ship inside golang.org/x/image.
// They cover Latin only. That makes them the right instrument for layout,
// determinism and safe-degradation tests, and it makes them useless as CJK
// evidence: the CJK typesetting claim can only be supported by a golden test
// driven by the manifest-pinned CJK face.
func latinLibrary(t *testing.T) *FontLibrary {
	t.Helper()
	library, err := NewFontLibrary(map[string][]byte{
		fontRoleRegular: goregular.TTF,
		fontRoleBold:    gobold.TTF,
		fontRoleItalic:  goitalic.TTF,
		fontRoleMono:    gomono.TTF,
	})
	if err != nil {
		t.Fatalf("the test font library did not load: %v", err)
	}
	t.Cleanup(func() { library.Close() })
	return library
}

func renderMarkdownFixture(t *testing.T, library *FontLibrary, source string) []byte {
	t.Helper()
	payload, err := RenderMarkdown([]byte(source), ScreenCapabilities{Width: 600, Height: 800}, library, DefaultMarkdownStyle())
	if err != nil {
		t.Fatalf("render failed for %q: %v", source, err)
	}
	return payload
}

func inkPixels(t *testing.T, payload []byte) int {
	t.Helper()
	decoded, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	gray, ok := decoded.(*image.Gray)
	if !ok {
		t.Fatalf("the render is not grayscale: %T", decoded)
	}
	count := 0
	for _, value := range gray.Pix {
		if value != 0xff {
			count++
		}
	}
	return count
}

func TestRenderMarkdownProducesAFullScreenGrayscalePNG(t *testing.T) {
	library := latinLibrary(t)
	screen := ScreenCapabilities{Width: 600, Height: 800}
	payload, err := RenderMarkdown([]byte("# Title\n\nBody text."), screen, library, DefaultMarkdownStyle())
	if err != nil {
		t.Fatal(err)
	}
	// The persistence layer only accepts a screen-sized grayscale PNG, so this
	// is the same gate the display transaction applies.
	if err := validateFullScreenPNG(payload, screen); err != nil {
		t.Fatalf("the render is not a valid full-screen frame: %v", err)
	}
	if inkPixels(t, payload) == 0 {
		t.Fatal("the render is blank")
	}
}

func TestRenderMarkdownIsDeterministic(t *testing.T) {
	library := latinLibrary(t)
	source := "# Heading\n\nA paragraph with **bold**, *italic* and `code` in it, long enough to wrap across more than one line of the panel.\n\n- first item\n- second item\n\n1. one\n2. two\n\n> quoted line\n\n```\ncode block line\n```\n\n---\n"
	first := renderMarkdownFixture(t, library, source)
	second := renderMarkdownFixture(t, latinLibrary(t), source)
	if !bytes.Equal(first, second) {
		t.Fatal("the same source produced two different frames")
	}
	if inkPixels(t, first) == 0 {
		t.Fatal("the frozen syntax set rendered nothing")
	}
}

func TestRenderMarkdownNeverExecutesHTMLOrFetchesRemoteResources(t *testing.T) {
	library := latinLibrary(t)
	blank := renderMarkdownFixture(t, library, "")

	// A raw HTML block is dropped outright: identical to an empty document, so
	// it is neither executed nor drawn.
	for _, source := range []string{"<script>alert(1)</script>", "<div onclick=\"x()\">hidden</div>", "<style>@import url(https://example.com/f.css);</style>"} {
		if got := renderMarkdownFixture(t, library, source); !bytes.Equal(got, blank) {
			t.Fatalf("raw HTML reached the frame: %q", source)
		}
	}

	// Inline HTML is stripped while the surrounding text survives.
	plain := renderMarkdownFixture(t, library, "Hello world")
	if got := renderMarkdownFixture(t, library, "Hello <b onmouseover=\"x()\">world</b>"); !bytes.Equal(got, plain) {
		t.Fatal("inline HTML changed the frame")
	}

	// A remote image degrades to its alt text; the destination is never used.
	altOnly := renderMarkdownFixture(t, library, "alt text")
	if got := renderMarkdownFixture(t, library, "![alt text](https://example.com/remote.png)"); !bytes.Equal(got, altOnly) {
		t.Fatal("a remote image was not degraded to its alt text")
	}

	// A link keeps its label and loses its destination.
	labelOnly := renderMarkdownFixture(t, library, "click here")
	if got := renderMarkdownFixture(t, library, "[click here](https://example.com/page)"); !bytes.Equal(got, labelOnly) {
		t.Fatal("a link destination reached the frame")
	}
}

func TestRenderMarkdownFailsClosedWhenAGlyphIsMissing(t *testing.T) {
	library := latinLibrary(t)
	// The Go fonts carry no Han glyphs. Rendering must report the failure so
	// the endpoint returns an error instead of committing a page of tofu boxes
	// as the last successful screen.
	_, err := RenderMarkdown([]byte("# 中文标题\n\n正文"), ScreenCapabilities{Width: 600, Height: 800}, library, DefaultMarkdownStyle())
	if !errors.Is(err, ErrMissingGlyph) {
		t.Fatalf("a document the fonts cannot draw did not fail closed: %v", err)
	}
}

func TestRenderMarkdownRequiresAFontLibraryAndAProbedScreen(t *testing.T) {
	library := latinLibrary(t)
	if _, err := RenderMarkdown([]byte("text"), ScreenCapabilities{Width: 600, Height: 800}, nil, DefaultMarkdownStyle()); !errors.Is(err, ErrFontMissing) {
		t.Fatal("rendering without fonts was allowed")
	}
	if _, err := RenderMarkdown([]byte("text"), ScreenCapabilities{}, library, DefaultMarkdownStyle()); err == nil {
		t.Fatal("rendering without a probed screen was allowed")
	}
}

func TestMarkdownWrappingBreaksBetweenCJKCharacters(t *testing.T) {
	// Line breaking is decided from measured advances, not from the font's
	// script coverage, so the rule itself is testable without a CJK face.
	cells := []glyphCell{{r: '中'}, {r: '文'}, {r: 'a'}, {r: 'b'}, {r: ' '}, {r: 'c'}}
	if !allowBreakAfter(cells, 0) {
		t.Fatal("no break opportunity between two Han characters")
	}
	if allowBreakAfter(cells, 2) {
		t.Fatal("a Latin word was broken mid-word")
	}
	if !allowBreakAfter(cells, 4) {
		t.Fatal("no break opportunity after a space")
	}
	closing := []glyphCell{{r: '中'}, {r: '。'}}
	if allowBreakAfter(closing, 0) {
		t.Fatal("a line was allowed to start with a closing mark")
	}
}
