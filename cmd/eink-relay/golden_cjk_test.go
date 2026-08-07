package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	"image/png"
	"os"
	"strings"
	"testing"
)

// repoFontManifestPath is the committed manifest, relative to this package. It
// is the file the installer copies into the device font directory, so testing
// it here tests exactly what ships.
const repoFontManifestPath = "../../assets/fonts/manifest.json"
const cjkGoldenSHA256Path = "testdata/golden_cjk.sha256"

// cjkGoldenPixelSHA256Path freezes the *decoded* frame rather than the PNG
// container. The container digest below still proves the whole pipeline is
// byte-deterministic, but it also moves whenever the encoder's compression
// settings change, which says nothing about typesetting. The pixel digest is
// the claim that actually matters — line breaks, metrics and full-screen
// geometry — and it is invariant under any lossless re-encoding, so a
// deliberate encoder change has to leave it untouched or be treated as a
// rendering regression.
const cjkGoldenPixelSHA256Path = "testdata/golden_cjk_pixels.sha256"

// TestCommittedFontManifestIsPinned always runs. It proves the redistribution
// record itself is well formed and pinned, independently of whether the font
// binary happens to be installed on this machine.
func TestCommittedFontManifestIsPinned(t *testing.T) {
	manifest, err := LoadFontManifest(repoFontManifestPath)
	if err != nil {
		t.Fatalf("the committed font manifest is unusable: %v", err)
	}
	if len(manifest.Fonts) != 1 {
		t.Fatalf("expected exactly one pinned font, got %d", len(manifest.Fonts))
	}
	entry := manifest.Fonts[0]
	if entry.Role != fontRoleRegular {
		t.Fatalf("the mandatory regular role is not pinned: %q", entry.Role)
	}
	if entry.Family == "" || entry.Style == "" || entry.Version == "" {
		t.Fatalf("the family, style or version is not pinned: %+v", entry)
	}
	// The exact face, not just "some Noto". Family plus style plus version is
	// what makes the redistribution record checkable by hand and what makes the
	// golden frames of later rounds reproducible: a different weight of the same
	// family has different metrics and would silently move every line break.
	if entry.Family != "Noto Sans CJK SC" || entry.Style != "Regular" || entry.Version != "Sans2.004" {
		t.Fatalf("the pinned face drifted from Noto Sans CJK SC / Regular / Sans2.004: %+v", entry)
	}
	if !strings.HasPrefix(entry.URL, "https://") || !strings.HasSuffix(entry.URL, entry.File) {
		t.Fatalf("the canonical URL does not resolve to the installed file name: %q", entry.URL)
	}
	if !strings.Contains(entry.License, "Open Font License") || entry.LicenseURL == "" {
		t.Fatalf("the licence record is incomplete: %+v", entry)
	}
	if entry.Bytes < 1 {
		t.Fatalf("the byte size is not pinned: %d", entry.Bytes)
	}
	// The digest shape is already enforced by ParseFontManifest; what matters
	// here is that the binary itself is not in the tree.
	if _, err := os.Stat("../../assets/fonts/" + entry.File); err == nil {
		t.Fatal("a font binary was committed to the repository")
	}
}

// installedCJKLibrary loads the manifest-pinned face from the configured font
// directory. The binary is deliberately not in Git, so the verification
// environment must provide it through EINKRELAY_FONT_DIR (or the device default
// path). Absence is a failure: a skipped font test is not CJK evidence.
func installedCJKLibrary(t *testing.T) *FontLibrary {
	t.Helper()
	manifest, err := LoadFontManifest(repoFontManifestPath)
	if err != nil {
		t.Fatalf("the committed font manifest is unusable: %v", err)
	}
	directory := os.Getenv("EINKRELAY_FONT_DIR")
	if directory == "" {
		directory = defaultFontDir
	}
	library, err := LoadFontLibrary(manifest, directory)
	if err != nil {
		t.Fatalf("the pinned CJK font is not installed in %s (%v); run `eink-relay fonts ensure` or provide EINKRELAY_FONT_DIR; CJK verification cannot be skipped", directory, err)
	}
	t.Cleanup(func() { library.Close() })
	return library
}

// framePixelDigest hashes the decoded grayscale samples and the geometry they
// belong to. Row slices are taken by width rather than by stride so the digest
// cannot depend on padding a decoder happens to leave behind.
func framePixelDigest(t *testing.T, payload []byte) string {
	t.Helper()
	decoded, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("the golden frame did not decode: %v", err)
	}
	gray, ok := decoded.(*image.Gray)
	if !ok {
		t.Fatalf("the golden frame is not 8-bit grayscale: %T", decoded)
	}
	width, height := gray.Rect.Dx(), gray.Rect.Dy()
	hasher := sha256.New()
	fmt.Fprintf(hasher, "gray %dx%d\n", width, height)
	for y := 0; y < height; y++ {
		start := gray.PixOffset(gray.Rect.Min.X, gray.Rect.Min.Y+y)
		hasher.Write(gray.Pix[start : start+width])
	}
	return fmt.Sprintf("%x", hasher.Sum(nil))
}

// TestGoldenCJKMixedTypesetting is the only test that can support the CJK
// typesetting claim: it drives the real renderer with the real manifest-pinned
// face. Because the renderer fails closed on any rune it cannot draw, a
// successful render is itself the assertion that no notdef box was painted.
func TestGoldenCJKMixedTypesetting(t *testing.T) {
	library := installedCJKLibrary(t)
	screen := ScreenCapabilities{Width: 758, Height: 1024}
	source := strings.Join([]string{
		"# 中文标题 with English",
		"",
		"这是一段中英文混排的正文，用来验证换行位置、字体度量和整屏几何在同一份源上是确定的：EInkRelay renders Markdown on the device itself.",
		"",
		"- 第一项：列表中的**粗体**与*斜体*",
		"- 第二项：inline `code` 与中文标点，。！？的禁则",
		"",
		"1. 有序列表第一条",
		"2. 有序列表第二条",
		"",
		"> 引用块：中文引用与 English quotation 混排。",
		"",
		"```",
		"code block 代码块",
		"```",
		"",
	}, "\n")

	first, err := RenderMarkdown([]byte(source), screen, library, DefaultMarkdownStyle())
	if err != nil {
		t.Fatalf("the pinned face could not render the mixed document: %v", err)
	}
	if err := validateFullScreenPNG(first, screen); err != nil {
		t.Fatalf("the golden frame is not a full-screen grayscale PNG: %v", err)
	}
	if inkPixels(t, first) == 0 {
		t.Fatal("the golden frame is blank")
	}
	expectedPixels, err := os.ReadFile(cjkGoldenPixelSHA256Path)
	if err != nil {
		t.Fatalf("read frozen CJK golden pixel digest: %v", err)
	}
	if got, want := framePixelDigest(t, first), strings.TrimSpace(string(expectedPixels)); got != want {
		t.Fatalf("CJK golden pixels changed: got %s, want %s; the typesetting itself moved — inspect the rendered layout before updating %s", got, want, cjkGoldenPixelSHA256Path)
	}

	expectedDigest, err := os.ReadFile(cjkGoldenSHA256Path)
	if err != nil {
		t.Fatalf("read frozen CJK golden digest: %v", err)
	}
	actualDigest := sha256.Sum256(first)
	if got, want := fmt.Sprintf("%x", actualDigest), strings.TrimSpace(string(expectedDigest)); got != want {
		t.Fatalf("CJK golden container changed: got %s, want %s; if the pixel digest above still matches, only the PNG encoding moved and %s may be refreshed", got, want, cjkGoldenSHA256Path)
	}

	// Determinism across a freshly parsed face: same source, same bytes. This is
	// what makes the line breaks and the geometry an assertion rather than an
	// observation.
	second, err := RenderMarkdown([]byte(source), screen, installedCJKLibrary(t), DefaultMarkdownStyle())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("the same CJK source produced two different frames")
	}

	// Bold, italic and monospace resolve to the same regular face while the
	// manifest pins only one file. That must still render, never fail and never
	// silently drop the emphasised text.
	emphasised, err := RenderMarkdown([]byte("**粗体中文** *斜体中文* `等宽中文`"), screen, library, DefaultMarkdownStyle())
	if err != nil {
		t.Fatalf("the role fallback failed on emphasised CJK: %v", err)
	}
	plain, err := RenderMarkdown([]byte("占位"), screen, library, DefaultMarkdownStyle())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(emphasised, plain) {
		t.Fatal("the emphasised document rendered as unrelated placeholder text")
	}
}
