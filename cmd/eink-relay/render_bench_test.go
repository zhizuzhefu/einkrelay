package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"testing"
)

// benchScreen is the PW4 panel. Every benchmark here targets it so the numbers
// are comparable with each other and with a device run.
var benchScreen = ScreenCapabilities{Width: 1072, Height: 1448}

// These benchmarks exist because the display pipeline is synchronous: every
// microsecond spent decoding, converting or encoding is a microsecond the
// caller waits with the panel unchanged, on a single-core ARMv7 CPU. They run
// on the development host, so the absolute numbers are optimistic by more than
// an order of magnitude; what they establish is the *ratio* between pipeline
// stages and between two implementations of the same stage, which is what a
// change to this code has to be judged on.
//
//	go test -run '^$' -bench . ./cmd/eink-relay
//
// The Markdown benchmarks need the manifest-pinned face and skip without it,
// because a benchmark of a substitute font measures nothing about this product.

func benchSourceImage(width, height int) image.Image {
	source := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			source.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: uint8(x ^ y), A: 0xff})
		}
	}
	return source
}

func benchPNGBody(tb testing.TB, width, height int) []byte {
	tb.Helper()
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, benchSourceImage(width, height)); err != nil {
		tb.Fatal(err)
	}
	return buffer.Bytes()
}

func benchJPEGBody(tb testing.TB, width, height int) []byte {
	tb.Helper()
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, benchSourceImage(width, height), &jpeg.Options{Quality: 90}); err != nil {
		tb.Fatal(err)
	}
	return buffer.Bytes()
}

func benchFontLibrary(b *testing.B) *FontLibrary {
	b.Helper()
	manifest, err := LoadFontManifest(repoFontManifestPath)
	if err != nil {
		b.Skipf("the committed font manifest is unusable: %v", err)
	}
	directory := os.Getenv("EINKRELAY_FONT_DIR")
	if directory == "" {
		directory = defaultFontDir
	}
	library, err := LoadFontLibrary(manifest, directory)
	if err != nil {
		b.Skipf("the pinned font is not installed in %s: %v", directory, err)
	}
	b.Cleanup(func() { library.Close() })
	return library
}

// BenchmarkRenderImagePNG and BenchmarkRenderImageJPEG are the whole
// request-path cost of one image: header inspection, decode, grayscale
// conversion, resample to the panel and encode.
func BenchmarkRenderImagePNG(b *testing.B) {
	body := benchPNGBody(b, 2000, 1500)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := RenderImage("image/png", "contain", body, benchScreen, DefaultImageLimits()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRenderImageJPEG(b *testing.B) {
	body := benchJPEGBody(b, 2000, 1500)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := RenderImage("image/jpeg", "cover", body, benchScreen, DefaultImageLimits()); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkToGray* isolate the conversion whose type switch is asserted exact
// by TestGrayFastPathsMatchTheGenericConversion, against the generic At/RGBA
// loop that remains the definition of the arithmetic. The gap between the two
// is the entire justification for the fast paths existing.
func BenchmarkToGrayNRGBAFast(b *testing.B) {
	benchToGray(b, benchDecodedPNG(b), false)
}

func BenchmarkToGrayNRGBAGeneric(b *testing.B) {
	benchToGray(b, benchDecodedPNG(b), true)
}

func BenchmarkToGrayYCbCrFast(b *testing.B) {
	benchToGray(b, benchDecodedJPEG(b), false)
}

func BenchmarkToGrayYCbCrGeneric(b *testing.B) {
	benchToGray(b, benchDecodedJPEG(b), true)
}

func benchDecodedPNG(b *testing.B) image.Image {
	b.Helper()
	decoded, err := png.Decode(bytes.NewReader(benchPNGBody(b, 2000, 1500)))
	if err != nil {
		b.Fatal(err)
	}
	return decoded
}

func benchDecodedJPEG(b *testing.B) image.Image {
	b.Helper()
	decoded, err := jpeg.Decode(bytes.NewReader(benchJPEGBody(b, 2000, 1500)))
	if err != nil {
		b.Fatal(err)
	}
	return decoded
}

func benchToGray(b *testing.B, source image.Image, generic bool) {
	bounds := source.Bounds()
	destination := image.NewGray(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if generic {
			toGrayGeneric(destination, source, bounds)
			continue
		}
		if !grayFastPath(destination, source, bounds) {
			b.Fatal("no fast path was taken")
		}
	}
}

// BenchmarkRenderMarkdown is the other request path: parse, measure, lay out,
// draw and encode a full panel of text.
func BenchmarkRenderMarkdown(b *testing.B) {
	library := benchFontLibrary(b)
	source := []byte(defaultHelpPageMarkdown)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := RenderMarkdown(source, benchScreen, library, defaultHelpPageStyle()); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEncodeScreen* is the evidence behind screenPNGEncoder's compression
// level. "Text" is the representative frame — mostly white with black glyphs,
// what a Markdown request produces; "Photo" is the worst case, a full panel of
// high-entropy pixels. Both report the resulting size so the time saved can be
// weighed against the bytes written to the device, rather than argued about.
func BenchmarkEncodeScreenTextBestSpeed(b *testing.B) {
	benchEncode(b, benchTextScreen(b), png.BestSpeed)
}

func BenchmarkEncodeScreenTextDefault(b *testing.B) {
	benchEncode(b, benchTextScreen(b), png.DefaultCompression)
}

func BenchmarkEncodeScreenPhotoBestSpeed(b *testing.B) {
	benchEncode(b, benchPhotoScreen(b), png.BestSpeed)
}

func BenchmarkEncodeScreenPhotoDefault(b *testing.B) {
	benchEncode(b, benchPhotoScreen(b), png.DefaultCompression)
}

func benchTextScreen(b *testing.B) *image.Gray {
	b.Helper()
	library := benchFontLibrary(b)
	payload, err := RenderMarkdown([]byte(defaultHelpPageMarkdown), benchScreen, library, defaultHelpPageStyle())
	if err != nil {
		b.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		b.Fatal(err)
	}
	return decoded.(*image.Gray)
}

func benchPhotoScreen(b *testing.B) *image.Gray {
	b.Helper()
	return toGray(benchSourceImage(benchScreen.Width, benchScreen.Height))
}

func benchEncode(b *testing.B, canvas *image.Gray, level png.CompressionLevel) {
	encoder := png.Encoder{CompressionLevel: level, BufferPool: pngBufferPool{}}
	encoded := 0
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buffer bytes.Buffer
		if err := encoder.Encode(&buffer, canvas); err != nil {
			b.Fatal(err)
		}
		encoded = buffer.Len()
	}
	b.ReportMetric(float64(encoded), "frame-bytes")
}

// BenchmarkValidateFullScreenPNG is the integrity re-decode every committed
// frame pays for. It is kept measurable because it is the one place where
// "prove the bytes are intact" and "answer the request quickly" trade off.
func BenchmarkValidateFullScreenPNG(b *testing.B) {
	canvas := benchPhotoScreen(b)
	var buffer bytes.Buffer
	if err := encodeScreenPNG(&buffer, canvas); err != nil {
		b.Fatal(err)
	}
	payload := buffer.Bytes()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := validateFullScreenPNG(payload, benchScreen); err != nil {
			b.Fatal(err)
		}
	}
}
