package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func grayFixture(t *testing.T, width, height int, shade func(x, y int) uint8) []byte {
	t.Helper()
	source := image.NewGray(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			source.Pix[y*source.Stride+x] = shade(x, y)
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, source); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func decodeGray(t *testing.T, body []byte) *image.Gray {
	t.Helper()
	decoded, err := png.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	gray, ok := decoded.(*image.Gray)
	if !ok {
		t.Fatalf("rendered image is not grayscale: %T", decoded)
	}
	return gray
}

// pngHeaderOnly builds a signature plus a well-formed IHDR chunk and nothing
// else. The pre-decode guard may only look at the declared geometry, so proving
// that a decode bomb is rejected must not require the bomb's pixel data.
func pngHeaderOnly(width, height uint32, depth, colorType, interlace byte) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = depth
	ihdr[9] = colorType
	ihdr[12] = interlace
	chunk := make([]byte, 4, 8+13+4)
	binary.BigEndian.PutUint32(chunk[0:4], 13)
	chunk = append(chunk, []byte("IHDR")...)
	chunk = append(chunk, ihdr...)
	sum := make([]byte, 4)
	binary.BigEndian.PutUint32(sum, crc32.ChecksumIEEE(chunk[4:]))
	chunk = append(chunk, sum...)
	return append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, chunk...)
}

// jpegSegment is one marker segment. The two-byte length field is supplied by
// jpegStream, so a fixture never has to keep it consistent by hand.
type jpegSegment struct {
	marker  byte
	payload []byte
}

// jpegFrameBodyWithIDs builds the payload of a baseline frame header whose
// component identifiers are given explicitly. The identifiers matter on their
// own: image/jpeg reads components named 'R', 'G', 'B' as a frame that is
// already RGB, which costs the same extra buffer as an Adobe transform of 0.
func jpegFrameBodyWithIDs(width, height uint16, ids ...byte) []byte {
	payload := []byte{8, 0, 0, 0, 0, byte(len(ids))}
	binary.BigEndian.PutUint16(payload[1:3], height)
	binary.BigEndian.PutUint16(payload[3:5], width)
	for _, id := range ids {
		payload = append(payload, id, 0x11, 0)
	}
	return payload
}

// jpegFrameBody builds the payload of a baseline frame header with the ordinary
// numbered component identifiers.
func jpegFrameBody(width, height uint16, components byte) []byte {
	ids := make([]byte, 0, components)
	for index := byte(0); index < components; index++ {
		ids = append(ids, index+1)
	}
	return jpegFrameBodyWithIDs(width, height, ids...)
}

// adobeBody builds the payload of an Adobe APP14 segment: the "Adobe" tag, a
// version, two flag words and finally the colour transform at offset 11.
func adobeBody(transform byte) []byte {
	return append([]byte("Adobe"), 0x00, 0x64, 0, 0, 0, 0, transform)
}

// jpegStream builds SOI, the given segments in order, and a start of scan
// marker. Terminating with SOS is what a real file does, so a fixture built this
// way exercises the header walk's normal exit rather than its exhaustion exit.
func jpegStream(segments ...jpegSegment) []byte {
	body := []byte{0xff, 0xd8}
	for _, segment := range segments {
		header := []byte{0xff, segment.marker, 0, 0}
		binary.BigEndian.PutUint16(header[2:4], uint16(len(segment.payload)+2))
		body = append(append(body, header...), segment.payload...)
	}
	return append(body, 0xff, 0xda)
}

// jpegHeaderOnly builds SOI plus a single frame header segment and stops there,
// so the header walk has to decide from an exhausted buffer rather than from a
// start of scan.
func jpegHeaderOnly(marker byte, width, height uint16, components byte) []byte {
	payload := jpegFrameBody(width, height, components)
	frame := []byte{0xff, 0xd8, 0xff, marker, 0, 0}
	binary.BigEndian.PutUint16(frame[4:6], uint16(len(payload)+2))
	return append(frame, payload...)
}

// jpegWithAdobeTransform places an Adobe APP14 segment either side of the frame
// header. A negative transform omits the segment altogether.
func jpegWithAdobeTransform(transform int, afterFrame bool, width, height uint16, components byte) []byte {
	frame := jpegSegment{marker: 0xc0, payload: jpegFrameBody(width, height, components)}
	if transform < 0 {
		return jpegStream(frame)
	}
	adobe := jpegSegment{marker: 0xee, payload: adobeBody(byte(transform))}
	if afterFrame {
		return jpegStream(frame, adobe)
	}
	return jpegStream(adobe, frame)
}

func TestRenderImageContainKeepsAspectAndPaintsWhiteMargins(t *testing.T) {
	screen := ScreenCapabilities{Width: 100, Height: 100}
	body := grayFixture(t, 40, 20, func(int, int) uint8 { return 0x00 })
	rendered, err := RenderImage("image/png", "contain", body, screen, DefaultImageLimits())
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeGray(t, rendered)
	if decoded.Rect.Dx() != screen.Width || decoded.Rect.Dy() != screen.Height {
		t.Fatalf("canvas does not match the panel: %v", decoded.Rect)
	}
	// 40x20 into 100x100 is width limited, so the content is a 100x50 band
	// centred vertically with white above and below.
	if decoded.Pix[decoded.PixOffset(50, 0)] != 0xff || decoded.Pix[decoded.PixOffset(50, 99)] != 0xff {
		t.Fatal("contain did not paint white margins")
	}
	if decoded.Pix[decoded.PixOffset(50, 50)] != 0x00 {
		t.Fatal("scaled content is missing from the centre band")
	}
}

func TestRenderImageCoverCropsCentredWithoutStretching(t *testing.T) {
	screen := ScreenCapabilities{Width: 100, Height: 100}
	// The outer thirds are black and the centre square is white. Cover must
	// crop the outer thirds away rather than squeeze them into the panel.
	body := grayFixture(t, 300, 100, func(x, _ int) uint8 {
		if x < 100 || x >= 200 {
			return 0x00
		}
		return 0xff
	})
	rendered, err := RenderImage("image/png", "cover", body, screen, DefaultImageLimits())
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeGray(t, rendered)
	if decoded.Rect.Dx() != screen.Width || decoded.Rect.Dy() != screen.Height {
		t.Fatalf("canvas does not match the panel: %v", decoded.Rect)
	}
	for _, x := range []int{0, 25, 50, 75, 99} {
		if decoded.Pix[decoded.PixOffset(x, 50)] != 0xff {
			t.Fatalf("cover retained cropped content at x=%d", x)
		}
	}
}

func TestPreDecodeGuardsRejectBeforeAllocation(t *testing.T) {
	screen := ScreenCapabilities{Width: 100, Height: 100}
	limits := DefaultImageLimits()
	cases := []struct {
		name      string
		mediaType string
		body      []byte
		want      error
	}{
		{name: "edge over 8192", mediaType: "image/png", body: pngHeaderOnly(9000, 10, 8, 0, 0), want: ErrImageDimensions},
		{name: "over 32 megapixels", mediaType: "image/png", body: pngHeaderOnly(8000, 8000, 8, 0, 0), want: ErrImageDimensions},
		{name: "decoded byte budget", mediaType: "image/png", body: pngHeaderOnly(5000, 5000, 8, 6, 0), want: ErrImageDimensions},
		{name: "interlaced png", mediaType: "image/png", body: pngHeaderOnly(16, 16, 8, 0, 1), want: ErrImageDimensions},
		{name: "progressive jpeg", mediaType: "image/jpeg", body: jpegHeaderOnly(0xc2, 16, 16, 3), want: ErrImageDimensions},
		{name: "cmyk jpeg", mediaType: "image/jpeg", body: jpegHeaderOnly(0xc0, 16, 16, 4), want: ErrImageDimensions},
		{name: "ycck jpeg declared after the frame header", mediaType: "image/jpeg", body: jpegWithAdobeTransform(2, true, 16, 16, 3), want: ErrImageDimensions},
		{name: "rgb jpeg over the byte budget", mediaType: "image/jpeg", body: jpegWithAdobeTransform(0, false, 4096, 3000, 3), want: ErrImageDimensions},
		{name: "two frame headers", mediaType: "image/jpeg", body: jpegStream(
			jpegSegment{marker: 0xc0, payload: jpegFrameBody(16, 16, 3)},
			jpegSegment{marker: 0xc0, payload: jpegFrameBody(32, 32, 3)},
		), want: ErrDecodeFailed},
		{name: "truncated png", mediaType: "image/png", body: []byte("this is not a png at all"), want: ErrDecodeFailed},
		{name: "unsupported media type", mediaType: "image/gif", body: grayFixture(t, 4, 4, func(int, int) uint8 { return 0 }), want: ErrDecodeFailed},
		{name: "unsupported fit", mediaType: "image/png", body: grayFixture(t, 4, 4, func(int, int) uint8 { return 0 }), want: ErrDecodeFailed},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fit := "contain"
			if test.name == "unsupported fit" {
				fit = "stretch"
			}
			rendered, err := RenderImage(test.mediaType, fit, test.body, screen, limits)
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
			if rendered != nil {
				t.Fatal("a rejected request produced a payload")
			}
		})
	}
}

// TestJPEGAdobeTransformIsObservedWhereverAPP14Sits pins the whole layout matrix
// rather than one example, because the position of the APP14 segment relative to
// the frame header is exactly what the header walk used to get wrong: it decided
// at the frame header, so a transform declared afterwards was invisible.
func TestJPEGAdobeTransformIsObservedWhereverAPP14Sits(t *testing.T) {
	layouts := []struct {
		name       string
		transform  int
		afterFrame bool
	}{
		{name: "no app14", transform: -1},
		{name: "transform 0 before the frame header", transform: 0},
		{name: "transform 1 before the frame header", transform: 1},
		{name: "transform 2 before the frame header", transform: 2},
		{name: "transform 0 after the frame header", transform: 0, afterFrame: true},
		{name: "transform 1 after the frame header", transform: 1, afterFrame: true},
		{name: "transform 2 after the frame header", transform: 2, afterFrame: true},
	}
	for _, layout := range layouts {
		for _, components := range []byte{1, 3, 4} {
			t.Run(fmt.Sprintf("%s/%d components", layout.name, components), func(t *testing.T) {
				body := jpegWithAdobeTransform(layout.transform, layout.afterFrame, 16, 24, components)
				header, err := inspectJPEG(body)
				// Four components is CMYK on its own; a transform of 2 is YCCK
				// however many components are declared alongside it.
				if components == 4 || layout.transform == 2 {
					if !errors.Is(err, ErrImageDimensions) {
						t.Fatalf("got %v, want %v", err, ErrImageDimensions)
					}
					return
				}
				if err != nil {
					t.Fatalf("a supported frame was rejected: %v", err)
				}
				if header.Width != 16 || header.Height != 24 {
					t.Fatalf("the declared geometry was misread: %+v", header)
				}
			})
		}
	}
}

// TestJPEGLastAdobeTransformBeforeTheScanGoverns pins the one case in which the
// full header walk is deliberately more permissive than the old early return.
// image/jpeg reprocesses every APP14 segment it meets, so the last transform
// before the scan is the one the decoder actually uses; a guard that decided at
// the first segment would disagree with the decoder in both directions.
func TestJPEGLastAdobeTransformBeforeTheScanGoverns(t *testing.T) {
	frame := jpegSegment{marker: 0xc0, payload: jpegFrameBody(16, 24, 3)}
	ycck := jpegSegment{marker: 0xee, payload: adobeBody(2)}
	ycbcr := jpegSegment{marker: 0xee, payload: adobeBody(1)}

	// The scan is reached with YCCK in force, so the decoder would build the
	// fourth plane however harmless the earlier segment looked.
	if _, err := inspectJPEG(jpegStream(ycbcr, frame, ycck)); !errors.Is(err, ErrImageDimensions) {
		t.Fatalf("a later YCCK declaration was ignored: got %v, want %v", err, ErrImageDimensions)
	}

	// Reversed, the decoder settles on an ordinary three-component transform, so
	// rejecting this would refuse an image it handles inside the byte budget.
	header, err := inspectJPEG(jpegStream(ycck, frame, ycbcr))
	if err != nil {
		t.Fatalf("a superseded YCCK declaration still rejected the frame: %v", err)
	}
	if header.Width != 16 || header.Height != 24 {
		t.Fatalf("the declared geometry was misread: %+v", header)
	}
}

// TestJPEGRGBFramesAreBudgetedAtTheirWorstCase pins the extra full-size
// image.RGBA that image/jpeg allocates in convertToRGB whenever isRGB() holds:
// a non-JFIF frame carrying an Adobe APP14 transform of 0, or one whose SOF
// component identifiers spell 'R', 'G', 'B'. Such a frame really costs three
// planes plus that RGBA buffer plus our grayscale target, eight bytes per pixel
// rather than the four an ordinary YCbCr frame costs.
//
// 4096x3000 is 12,288,000 pixels: inside the 8192 edge and inside the
// 32-megapixel limit, roughly 49 MB at four bytes per pixel and roughly 98 MB at
// eight. The byte budget is therefore provably the only guard that can decide
// these cases, in either direction.
func TestJPEGRGBFramesAreBudgetedAtTheirWorstCase(t *testing.T) {
	const width, height = 4096, 3000
	// Stated rather than defaulted: this test is about the estimator widening
	// for the RGB path, and the default budget is a device measurement that is
	// free to move without changing what is being asserted here.
	limits := ImageLimits{MaxDimension: 8192, MaxPixels: 32000000, MaxDecodedBytes: 64 * 1024 * 1024}

	rejected := []struct {
		name string
		body []byte
	}{
		{name: "adobe transform 0 before the frame header", body: jpegWithAdobeTransform(0, false, width, height, 3)},
		{name: "adobe transform 0 after the frame header", body: jpegWithAdobeTransform(0, true, width, height, 3)},
		{name: "rgb component identifiers", body: jpegStream(
			jpegSegment{marker: 0xc0, payload: jpegFrameBodyWithIDs(width, height, 'R', 'G', 'B')},
		)},
	}
	for _, test := range rejected {
		t.Run(test.name, func(t *testing.T) {
			header, err := inspectJPEG(test.body)
			if err != nil {
				t.Fatalf("the header walk failed before the budget could be applied: %v", err)
			}
			if header.Width != width || header.Height != height {
				t.Fatalf("the declared geometry was misread: %+v", header)
			}
			if err := checkImageHeader(header, limits); !errors.Is(err, ErrImageDimensions) {
				t.Fatalf("got %v, want %v (estimate %d)", err, ErrImageDimensions, header.DecodedBytes)
			}
		})
	}

	// The ordinary envelope must not shrink. The same geometry declared as plain
	// YCbCr stays inside the budget, so the wider estimate above is attributable
	// to the RGB path alone and not to a blanket tightening.
	accepted := []struct {
		name string
		body []byte
	}{
		{name: "adobe transform 1", body: jpegWithAdobeTransform(1, false, width, height, 3)},
		{name: "adobe transform 1 after the frame header", body: jpegWithAdobeTransform(1, true, width, height, 3)},
		{name: "no app14", body: jpegWithAdobeTransform(-1, false, width, height, 3)},
	}
	for _, test := range accepted {
		t.Run(test.name, func(t *testing.T) {
			header, err := inspectJPEG(test.body)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkImageHeader(header, limits); err != nil {
				t.Fatalf("an ordinary YCbCr frame was rejected: %v (estimate %d)", err, header.DecodedBytes)
			}
		})
	}

	// Reading the identifiers means indexing three bytes per declared component.
	// A frame header that declares three and then stops must be refused rather
	// than indexed past its end.
	truncated := jpegFrameBodyWithIDs(width, height, 'R', 'G', 'B')[:8]
	if _, err := inspectJPEG(jpegStream(jpegSegment{marker: 0xc0, payload: truncated})); !errors.Is(err, ErrDecodeFailed) {
		t.Fatalf("a truncated frame header was accepted: got %v, want %v", err, ErrDecodeFailed)
	}
}

func TestDeclaredDimensionsWithinBudgetAreAccepted(t *testing.T) {
	// This is about the estimator, not about where the budget happens to sit,
	// so the budget is stated here rather than taken from the default: the
	// default is a device measurement and is free to move without invalidating
	// what the estimator does.
	//
	// 5000x5000 grayscale is 25 megapixels and roughly 50MiB of estimate. Only
	// the four-channel variant of the same geometry is rejected, which is what
	// proves the byte budget is doing the rejecting rather than the pixel cap.
	limits := ImageLimits{MaxDimension: 8192, MaxPixels: 32000000, MaxDecodedBytes: 64 * 1024 * 1024}
	header, err := inspectPNG(pngHeaderOnly(5000, 5000, 8, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkImageHeader(header, limits); err != nil {
		t.Fatalf("grayscale 5000x5000 was rejected: %v", err)
	}
	if header.DecodedBytes <= 0 || header.DecodedBytes > limits.MaxDecodedBytes {
		t.Fatalf("unexpected budget estimate: %d", header.DecodedBytes)
	}
}

func TestRenderImageIsDeterministic(t *testing.T) {
	screen := ScreenCapabilities{Width: 71, Height: 53}
	body := grayFixture(t, 37, 91, func(x, y int) uint8 { return uint8((x*7 + y*13) % 256) })
	first, err := RenderImage("image/png", "contain", body, screen, DefaultImageLimits())
	if err != nil {
		t.Fatal(err)
	}
	second, err := RenderImage("image/png", "contain", body, screen, DefaultImageLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("rendering the same payload twice produced different bytes")
	}
	decoded := decodeGray(t, first)
	if decoded.Rect.Dx() != screen.Width || decoded.Rect.Dy() != screen.Height {
		t.Fatalf("odd geometry was not honoured: %v", decoded.Rect)
	}
}

func TestLoadImageLimitsFromEnv(t *testing.T) {
	limits, err := LoadImageLimitsFromEnv(func(string) string { return "" })
	if err != nil || limits != DefaultImageLimits() || limits.MaxDecodedBytes != 50331648 {
		t.Fatalf("unexpected defaults: %+v %v", limits, err)
	}
	environment := map[string]string{"EINKRELAY_IMAGE_MAX_DECODED_BYTES": "1048576"}
	loaded, err := LoadImageLimitsFromEnv(func(name string) string { return environment[name] })
	if err != nil || loaded.MaxDecodedBytes != 1048576 {
		t.Fatalf("override was not applied: %+v %v", loaded, err)
	}
	for _, invalid := range []string{"0", "-1", "not-a-number"} {
		environment["EINKRELAY_IMAGE_MAX_DECODED_BYTES"] = invalid
		if _, err := LoadImageLimitsFromEnv(func(name string) string { return environment[name] }); err == nil {
			t.Fatalf("accepted invalid budget %q", invalid)
		}
	}
}

// grayConversionFixtures builds one image of every concrete type the two
// supported decoders can hand toGray, plus the 16-bit and sub-image cases the
// PNG decoder reaches. Values are deliberately spread across the whole range,
// including fully transparent, fully opaque and partial alpha, because the
// composite-over-white step is where a fast path is most likely to drift.
func grayConversionFixtures() map[string]image.Image {
	const width, height = 23, 17
	sample := func(x, y, salt int) uint8 { return uint8((x*37 + y*61 + salt*13) & 0xff) }

	gray := image.NewGray(image.Rect(0, 0, width, height))
	gray16 := image.NewGray16(image.Rect(0, 0, width, height))
	rgba := image.NewRGBA(image.Rect(0, 0, width, height))
	nrgba := image.NewNRGBA(image.Rect(0, 0, width, height))
	rgba64 := image.NewRGBA64(image.Rect(0, 0, width, height))
	nrgba64 := image.NewNRGBA64(image.Rect(0, 0, width, height))
	paletted := image.NewPaletted(image.Rect(0, 0, width, height), nil)
	ycbcr := image.NewYCbCr(image.Rect(0, 0, width, height), image.YCbCrSubsampleRatio420)

	palette := make(color.Palette, 0, 256)
	for index := 0; index < 256; index++ {
		palette = append(palette, color.NRGBA{
			R: uint8(index), G: uint8(255 - index), B: uint8(index * 7), A: uint8(index | 0x0f),
		})
	}
	paletted.Palette = palette

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			gray.SetGray(x, y, color.Gray{Y: sample(x, y, 0)})
			gray16.SetGray16(x, y, color.Gray16{Y: uint16(sample(x, y, 1))<<8 | uint16(sample(x, y, 2))})
			// A premultiplied colour must never exceed its own alpha, so the
			// channels are masked down rather than set independently.
			alpha := sample(x, y, 3)
			clamp := func(value uint8) uint8 { return uint8(int(value) % (int(alpha) + 1)) }
			rgba.SetRGBA(x, y, color.RGBA{R: clamp(sample(x, y, 4)), G: clamp(sample(x, y, 5)), B: clamp(sample(x, y, 6)), A: alpha})
			nrgba.SetNRGBA(x, y, color.NRGBA{R: sample(x, y, 7), G: sample(x, y, 8), B: sample(x, y, 9), A: alpha})
			wide := uint16(alpha)<<8 | uint16(alpha)
			rgba64.SetRGBA64(x, y, color.RGBA64{R: wide / 3, G: wide / 5, B: wide / 7, A: wide})
			nrgba64.SetNRGBA64(x, y, color.NRGBA64{R: uint16(sample(x, y, 10)) << 8, G: uint16(sample(x, y, 11)) << 8, B: uint16(sample(x, y, 12)) << 8, A: wide})
			paletted.SetColorIndex(x, y, sample(x, y, 13))
			ycbcr.Y[ycbcr.YOffset(x, y)] = sample(x, y, 14)
			offset := ycbcr.COffset(x, y)
			ycbcr.Cb[offset] = sample(x, y, 15)
			ycbcr.Cr[offset] = sample(x, y, 16)
		}
	}

	return map[string]image.Image{
		"Gray":     gray,
		"Gray16":   gray16,
		"RGBA":     rgba,
		"NRGBA":    nrgba,
		"RGBA64":   rgba64,
		"NRGBA64":  nrgba64,
		"Paletted": paletted,
		"YCbCr":    ycbcr,
		// A sub-image has a non-zero origin, which is the case where the
		// per-type offset arithmetic differs from the generic At(x, y) form.
		"NRGBASubImage": nrgba.SubImage(image.Rect(3, 2, width-4, height-3)),
		"GraySubImage":  gray.SubImage(image.Rect(5, 1, width-2, height-6)),
		"YCbCrSubImage": ycbcr.SubImage(image.Rect(4, 4, width-5, height-5)),
	}
}

// TestGrayFastPathsMatchTheGenericConversion is the contract behind toGray's
// type switch: every fast path must be the generic At/RGBA loop, not an
// approximation of it. A single differing byte here would move real pixels on
// the panel and silently invalidate the frozen golden frame.
func TestGrayFastPathsMatchTheGenericConversion(t *testing.T) {
	for name, source := range grayConversionFixtures() {
		bounds := source.Bounds()
		fast := toGray(source)
		reference := image.NewGray(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
		toGrayGeneric(reference, source, bounds)
		if !bytes.Equal(fast.Pix, reference.Pix) {
			for index := range reference.Pix {
				if fast.Pix[index] != reference.Pix[index] {
					t.Fatalf("%s: fast path diverged at pixel %d: got %d, want %d", name, index, fast.Pix[index], reference.Pix[index])
				}
			}
			t.Fatalf("%s: fast path produced a differently sized buffer", name)
		}
	}
}

// TestGrayFastPathIsTakenForDecoderOutputs pins the fast path to the types that
// actually arrive on the request path. If a future refactor made one of them
// fall through to the generic loop the output would stay correct and the
// regression would be invisible, so it is asserted directly.
func TestGrayFastPathIsTakenForDecoderOutputs(t *testing.T) {
	for name, source := range grayConversionFixtures() {
		bounds := source.Bounds()
		destination := image.NewGray(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
		if !grayFastPath(destination, source, bounds) {
			t.Fatalf("%s: no fast path was taken", name)
		}
	}
}

// TestGrayPalettedIndexBeyondThePaletteIsWhite covers the one place the fast
// path deliberately differs from image.Paletted.At, which panics on an index
// past the end of the palette. A malformed index is not a reason to take a
// display service down.
func TestGrayPalettedIndexBeyondThePaletteIsWhite(t *testing.T) {
	source := image.NewPaletted(image.Rect(0, 0, 2, 1), color.Palette{color.Gray{Y: 0x00}})
	source.Pix[1] = 200
	converted := toGray(source)
	if converted.Pix[0] != 0x00 {
		t.Fatalf("a valid palette index was not converted: %d", converted.Pix[0])
	}
	if converted.Pix[1] != 0xff {
		t.Fatalf("an out-of-palette index should resolve to white, got %d", converted.Pix[1])
	}
}

// TestScreenPNGEncoderRoundTripsEveryFrame guards the encoder swap: whatever
// compression level the display pipeline uses, the bytes it emits must still be
// a full-screen 8-bit grayscale PNG that decodes back to the exact samples that
// went in. PNG is lossless, so "faster" must never become "different".
func TestScreenPNGEncoderRoundTripsEveryFrame(t *testing.T) {
	screen := ScreenCapabilities{Width: 61, Height: 43}
	canvas := image.NewGray(image.Rect(0, 0, screen.Width, screen.Height))
	for index := range canvas.Pix {
		canvas.Pix[index] = uint8(index * 7 % 251)
	}
	var buffer bytes.Buffer
	if err := encodeScreenPNG(&buffer, canvas); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := validateFullScreenPNG(buffer.Bytes(), screen); err != nil {
		t.Fatalf("the encoded frame is not a valid full-screen frame: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(buffer.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(decoded.(*image.Gray).Pix, canvas.Pix) {
		t.Fatal("the round trip changed the samples")
	}
	// The pooled encoder buffer is reused across calls; a second frame must not
	// inherit anything from the first.
	var again bytes.Buffer
	if err := encodeScreenPNG(&again, canvas); err != nil {
		t.Fatalf("second encode: %v", err)
	}
	if !bytes.Equal(buffer.Bytes(), again.Bytes()) {
		t.Fatal("the pooled encoder produced two different frames for one input")
	}
}

// TestDecodeBudgetDefaultIsTheMeasuredOne guards a value that came from device
// measurements rather than from a round number, and that was corrected once
// after the first reading of those measurements turned out to be wrong: `free`
// excludes reclaimable page cache, and MemAvailable — the figure that actually
// governs an out-of-memory kill — never dropped below about 110MB. What
// justifies the budget being tighter than the original 64MiB is not a cliff but
// the exclusive-mode redesign: the native interface is no longer stopped while
// the panel is covered, so peak headroom is shared. Moving this is a deliberate
// act, not something to drift into.
func TestDecodeBudgetDefaultIsTheMeasuredOne(t *testing.T) {
	const measured = 48 * 1024 * 1024
	if DefaultImageLimits().MaxDecodedBytes != measured {
		t.Fatalf("default decode budget = %d, want the measured %d", DefaultImageLimits().MaxDecodedBytes, measured)
	}
	// The budget has to be the binding constraint for colour images well before
	// the pixel ceiling is: a three-component frame costs four bytes per pixel
	// through this pipeline, so the pixel cap alone would allow far more than
	// the device can hold.
	limits := DefaultImageLimits()
	pixelsAllowedByBudget := limits.MaxDecodedBytes / 4
	if pixelsAllowedByBudget >= limits.MaxPixels {
		t.Fatalf("the decode budget (%d px worth) no longer binds before the pixel ceiling (%d px)", pixelsAllowedByBudget, limits.MaxPixels)
	}
}
