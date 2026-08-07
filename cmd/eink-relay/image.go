package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"sync"
)

var (
	// ErrImageDimensions is decided from the declared header alone, before any
	// buffer proportional to those dimensions is allocated. It maps to the
	// frozen 413 image_dimensions_exceeded response.
	ErrImageDimensions = errors.New("image dimensions exceeded")
	// ErrDecodeFailed maps to the frozen 422 decode_failed response.
	ErrDecodeFailed = errors.New("image decode failed")
)

// defaultImageMaxDecodedBytes caps the worst-case decode allocation. The frozen
// 8192-edge and 32-megapixel limits bound pixel counts but not bytes: a
// standards-compliant image inside both of them can still ask the standard
// library for hundreds of megabytes, which on a ~490MB ARMv7 device is a fatal
// out-of-memory exit rather than a 500 response.
//
// The value is 48MiB, measured on the device. What the measurements say, in
// full, because the first reading of them was wrong:
//
//	request              peak RSS   MemAvailable
//	idle                   20MB       167MB
//	3.0MP                  35MB       155MB
//	6.2MP                  43MB       147MB
//	12.2MP (49MB est.)     76MB       ~111MB
//
// An earlier revision cut this to 32MiB on the grounds that a 12.2MP frame left
// the device with 10MB free. That was a misread: `free` excludes reclaimable
// page cache, and the figure that governs an out-of-memory kill is
// MemAvailable, which stayed above 110MB throughout. There was no cliff, and
// rejecting an ordinary 12-megapixel phone photo to avoid one was a bad trade.
//
// It is still tighter than the 64MiB it started at, for a reason that only
// became true with the exclusive-mode redesign: the native interface is no
// longer stopped while the panel is covered. Xorg, awesome and the framework
// daemon stay resident and can allocate at any moment — opening a book, drawing
// a dialog — so peak headroom is no longer this process's to spend alone. 48MiB
// admits the 12.2MP case at a 76MB peak and refuses the 17.6MP one, which is
// where the estimator and the measurements agree the curve gets steep.
//
// The estimator itself is sound: observed peak tracks the declared estimate at
// about 1.15x across the range above.
const defaultImageMaxDecodedBytes int64 = 48 * 1024 * 1024

// ImageLimits bounds a request before it is decoded.
type ImageLimits struct {
	MaxDimension    int
	MaxPixels       int64
	MaxDecodedBytes int64
}

func DefaultImageLimits() ImageLimits {
	return ImageLimits{MaxDimension: 8192, MaxPixels: 32000000, MaxDecodedBytes: defaultImageMaxDecodedBytes}
}

func LoadImageLimitsFromEnv(getenv func(string) string) (ImageLimits, error) {
	limits := DefaultImageLimits()
	if err := envInt(getenv, "EINKRELAY_IMAGE_MAX_DIMENSION", &limits.MaxDimension); err != nil {
		return ImageLimits{}, err
	}
	if err := envInt64(getenv, "EINKRELAY_IMAGE_MAX_PIXELS", &limits.MaxPixels); err != nil {
		return ImageLimits{}, err
	}
	if err := envInt64(getenv, "EINKRELAY_IMAGE_MAX_DECODED_BYTES", &limits.MaxDecodedBytes); err != nil {
		return ImageLimits{}, err
	}
	if limits.MaxDimension < 1 || limits.MaxPixels < 1 || limits.MaxDecodedBytes < 1 {
		return ImageLimits{}, errors.New("invalid configuration")
	}
	return limits, nil
}

// imageHeader carries only what the declared header states. Every field is
// int64 because a PNG header may declare up to 2^32-1 and int is 32 bits on the
// ARMv7 target.
type imageHeader struct {
	Width        int64
	Height       int64
	DecodedBytes int64
}

var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// inspectPNG reads the fixed-size IHDR chunk. Adam7 interlacing is refused here
// because it forces the decoder to hold the whole image plus per-pass buffers,
// and png.DecodeConfig does not expose the interlace method.
func inspectPNG(body []byte) (imageHeader, error) {
	if len(body) < 8+8+13+4 || !bytes.Equal(body[:8], pngSignature) {
		return imageHeader{}, ErrDecodeFailed
	}
	if binary.BigEndian.Uint32(body[8:12]) != 13 || string(body[12:16]) != "IHDR" {
		return imageHeader{}, ErrDecodeFailed
	}
	ihdr := body[16:29]
	width := int64(binary.BigEndian.Uint32(ihdr[0:4]))
	height := int64(binary.BigEndian.Uint32(ihdr[4:8]))
	depth := int64(ihdr[8])
	colorType := ihdr[9]
	if width < 1 || height < 1 {
		return imageHeader{}, ErrDecodeFailed
	}
	switch depth {
	case 1, 2, 4, 8, 16:
	default:
		return imageHeader{}, ErrDecodeFailed
	}
	var channels, perPixel int64
	switch colorType {
	case 0:
		channels, perPixel = 1, 1
		if depth == 16 {
			perPixel = 2
		}
	case 2:
		channels, perPixel = 3, 4
		if depth == 16 {
			perPixel = 8
		}
	case 3:
		channels, perPixel = 1, 1
	case 4:
		channels, perPixel = 2, 4
		if depth == 16 {
			perPixel = 8
		}
	case 6:
		channels, perPixel = 4, 4
		if depth == 16 {
			perPixel = 8
		}
	default:
		return imageHeader{}, ErrDecodeFailed
	}
	if ihdr[12] != 0 {
		return imageHeader{}, ErrImageDimensions
	}
	rowBytes := (width*channels*depth + 7) / 8
	// Decoded image plus our grayscale target plus the two filter rows the
	// decoder keeps. Deliberately an over-estimate.
	estimate := width*height*perPixel + width*height + 2*(rowBytes+1)
	return imageHeader{Width: width, Height: height, DecodedBytes: estimate}, nil
}

// inspectJPEG walks the marker segments up to the start of scan. Progressive and
// CMYK/YCCK frames are refused because both multiply the decoder's working set
// far beyond what the pixel count suggests.
//
// The whole header is walked before anything is decided. An Adobe APP14 segment
// is free to sit after the frame header, and the standard library honours the
// last transform it saw, so returning at the frame header would let a YCCK
// declaration travel past this guard.
func inspectJPEG(body []byte) (imageHeader, error) {
	if len(body) < 4 || body[0] != 0xff || body[1] != 0xd8 {
		return imageHeader{}, ErrDecodeFailed
	}
	adobeTransform := -1
	var frame []byte
	frameSeen := false
	offset := 2
	for offset+1 < len(body) {
		if body[offset] != 0xff {
			return imageHeader{}, ErrDecodeFailed
		}
		for offset < len(body) && body[offset] == 0xff {
			offset++
		}
		if offset >= len(body) {
			return imageHeader{}, ErrDecodeFailed
		}
		marker := body[offset]
		offset++
		if marker == 0x00 {
			return imageHeader{}, ErrDecodeFailed
		}
		if marker == 0x01 || marker == 0xd8 || (marker >= 0xd0 && marker <= 0xd7) {
			continue
		}
		if marker == 0xd9 || marker == 0xda {
			// End of image or start of scan. Entropy-coded data is not marker
			// aligned, so a header walk cannot continue past this point.
			if !frameSeen {
				return imageHeader{}, ErrDecodeFailed
			}
			return jpegFrame(frame, adobeTransform)
		}
		if offset+2 > len(body) {
			return imageHeader{}, ErrDecodeFailed
		}
		length := int(binary.BigEndian.Uint16(body[offset : offset+2]))
		if length < 2 || offset+length > len(body) {
			return imageHeader{}, ErrDecodeFailed
		}
		segment := body[offset+2 : offset+length]
		switch marker {
		case 0xee:
			if len(segment) >= 12 && string(segment[:5]) == "Adobe" {
				adobeTransform = int(segment[11])
			}
		case 0xc2:
			return imageHeader{}, ErrImageDimensions
		case 0xc0, 0xc1:
			if frameSeen {
				// The standard library refuses a second frame header, and a
				// budget derived from one of two frames guarantees nothing about
				// what the decoder would then allocate.
				return imageHeader{}, ErrDecodeFailed
			}
			// The segment aliases body, which is never written, so holding it
			// until the walk ends is cheaper and safer than re-deriving the
			// geometry from copied fields.
			frame = segment
			frameSeen = true
		case 0xc3, 0xc5, 0xc6, 0xc7, 0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf:
			// Lossless, differential and arithmetic frames; unsupported.
			return imageHeader{}, ErrDecodeFailed
		}
		offset += length
	}
	// A header that ends without a start of scan is still decidable as long as
	// the frame header itself was complete.
	if !frameSeen {
		return imageHeader{}, ErrDecodeFailed
	}
	return jpegFrame(frame, adobeTransform)
}

func jpegFrame(segment []byte, adobeTransform int) (imageHeader, error) {
	if len(segment) < 6 {
		return imageHeader{}, ErrDecodeFailed
	}
	height := int64(binary.BigEndian.Uint16(segment[1:3]))
	width := int64(binary.BigEndian.Uint16(segment[3:5]))
	components := int64(segment[5])
	if width < 1 || height < 1 {
		return imageHeader{}, ErrDecodeFailed
	}
	// Four components means CMYK; an Adobe transform of 2 means YCCK. Both
	// need a fourth full-resolution plane and an extra colour conversion.
	if components == 4 || adobeTransform == 2 {
		return imageHeader{}, ErrImageDimensions
	}
	if components != 1 && components != 3 {
		return imageHeader{}, ErrDecodeFailed
	}
	// Each component contributes three bytes after the count: identifier,
	// sampling factors and quantisation table. A frame header that stops short
	// of them declares components it does not describe, so nothing about the
	// decoder's allocation can be concluded from it.
	if len(segment) < 6+3*int(components) {
		return imageHeader{}, ErrDecodeFailed
	}
	// One full-resolution plane per component (the 4:4:4 worst case) plus our
	// grayscale target.
	estimate := width*height*components + width*height
	// image/jpeg treats a three-component frame as already-RGB when an Adobe
	// APP14 declares transform 0, or when the component identifiers spell
	// 'R', 'G', 'B'. It then allocates a further full image.RGBA in
	// convertToRGB, so the true cost is the planes plus four more bytes per
	// pixel plus our target: eight rather than four. The identifiers sit at
	// offsets 6, 9 and 12, which the length check above guarantees exist.
	//
	// A JFIF APP0 suppresses that path in the standard library and we do not
	// track APP0. Over-estimating a JFIF frame that also carries an Adobe
	// transform of 0 is the conservative direction and is deliberate, matching
	// the over-estimate stance taken everywhere else in this guard.
	if components == 3 && (adobeTransform == 0 || (segment[6] == 'R' && segment[9] == 'G' && segment[12] == 'B')) {
		estimate += width * height * 4
	}
	return imageHeader{Width: width, Height: height, DecodedBytes: estimate}, nil
}

func checkImageHeader(header imageHeader, limits ImageLimits) error {
	if header.Width > int64(limits.MaxDimension) || header.Height > int64(limits.MaxDimension) {
		return ErrImageDimensions
	}
	if header.Width*header.Height > limits.MaxPixels {
		return ErrImageDimensions
	}
	if header.DecodedBytes > limits.MaxDecodedBytes {
		return ErrImageDimensions
	}
	return nil
}

// RenderImage validates and rasterises a PNG or JPEG payload into a full-screen
// grayscale PNG whose geometry matches the probed panel exactly. The media type
// is the one already validated by the HTTP layer; content sniffing is
// deliberately not used, so a payload cannot escape the guard that its declared
// type implies.
func RenderImage(mediaType, fit string, body []byte, screen ScreenCapabilities, limits ImageLimits) ([]byte, error) {
	if screen.Width < 1 || screen.Height < 1 {
		return nil, errors.New("screen geometry is unavailable")
	}
	if fit != "contain" && fit != "cover" {
		return nil, ErrDecodeFailed
	}
	var header imageHeader
	var err error
	switch mediaType {
	case "image/png":
		header, err = inspectPNG(body)
	case "image/jpeg":
		header, err = inspectJPEG(body)
	default:
		return nil, ErrDecodeFailed
	}
	if err != nil {
		return nil, err
	}
	if err := checkImageHeader(header, limits); err != nil {
		return nil, err
	}
	var decoded image.Image
	if mediaType == "image/png" {
		decoded, err = png.Decode(bytes.NewReader(body))
	} else {
		decoded, err = jpeg.Decode(bytes.NewReader(body))
	}
	if err != nil {
		return nil, ErrDecodeFailed
	}
	bounds := decoded.Bounds()
	if int64(bounds.Dx()) != header.Width || int64(bounds.Dy()) != header.Height {
		// The budget was computed from the declared header; a decoder that
		// produced something else invalidates that guarantee.
		return nil, ErrDecodeFailed
	}
	gray := toGray(decoded)
	var full *image.Gray
	if fit == "cover" {
		full = fitCover(gray, screen)
	} else {
		full = fitContain(gray, screen)
	}
	var buffer bytes.Buffer
	if err := encodeScreenPNG(&buffer, full); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// screenPNGEncoder encodes every frame that reaches the panel.
//
// BestSpeed rather than DefaultCompression: the frame is a transient candidate
// file consumed by FBInk on the same device, so the only thing its compression
// ratio buys is a slightly smaller write, while its compression *time* sits
// directly on the synchronous request path of a single-core ARMv7 CPU. On the
// representative full-screen frame — mostly white with black text, which is
// what a Markdown request produces — BestSpeed is measured at roughly half the
// encode time for 5% more bytes. Photographic content compresses worse in
// absolute terms but gains proportionally more time back, and the extra bytes
// are still bounded by one screen of 8-bit grayscale.
//
// The buffer pool keeps the encoder's per-call scratch allocations off a
// ~490MB device. It is safe to share: the pool is concurrency-safe and the
// handler's display latch already admits one transaction at a time.
var screenPNGEncoder = png.Encoder{
	CompressionLevel: png.BestSpeed,
	BufferPool:       pngBufferPool{},
}

type pngBufferPool struct{}

var pngBuffers sync.Pool

func (pngBufferPool) Get() *png.EncoderBuffer {
	buffer, _ := pngBuffers.Get().(*png.EncoderBuffer)
	return buffer
}

func (pngBufferPool) Put(buffer *png.EncoderBuffer) { pngBuffers.Put(buffer) }

var _ png.EncoderBufferPool = pngBufferPool{}

func encodeScreenPNG(buffer *bytes.Buffer, frame *image.Gray) error {
	return screenPNGEncoder.Encode(buffer, frame)
}

// toGray flattens the image onto white before converting to luminance. The
// compositing step matters because RGBA() returns premultiplied values, so a
// transparent pixel would otherwise darken to black and contradict the white
// margins that contain is required to produce.
//
// The concrete types the two supported decoders can produce are converted by
// grayFastPath, which reads the pixel buffers directly. The generic loop below
// stays as the fallback and as the definition of the arithmetic: every fast
// path reproduces it exactly, which is what
// TestGrayFastPathsMatchTheGenericConversion asserts pixel by pixel.
func toGray(src image.Image) *image.Gray {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	dst := image.NewGray(image.Rect(0, 0, width, height))
	if grayFastPath(dst, src, bounds) {
		return dst
	}
	toGrayGeneric(dst, src, bounds)
	return dst
}

func toGrayGeneric(dst *image.Gray, src image.Image, bounds image.Rectangle) {
	width, height := bounds.Dx(), bounds.Dy()
	for y := 0; y < height; y++ {
		row := dst.Pix[y*dst.Stride : y*dst.Stride+width]
		for x := 0; x < width; x++ {
			r, g, b, a := src.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			row[x] = grayFromPremultiplied(r, g, b, a)
		}
	}
}

// grayFromPremultiplied is the shared arithmetic: composite the premultiplied
// colour over white, then take the ITU-R BT.601 luma with half-up rounding.
// Integer only, so the result is identical on darwin/arm64 and linux/arm.
func grayFromPremultiplied(r, g, b, a uint32) uint8 {
	r += 0xffff - a
	g += 0xffff - a
	b += 0xffff - a
	return uint8((19595*r + 38470*g + 7471*b + 1<<15) >> 24)
}

// grayFastPath converts without going through image.Image.At, which costs an
// interface dispatch and a colour boxing per pixel. It reports whether it
// handled the type; anything it does not recognise falls back to the generic
// loop rather than being converted approximately.
//
// Each branch reads the pixel buffer directly and then hands the samples to the
// *same* color.Color.RGBA method the generic path would have reached. Copying
// that arithmetic inline would be marginally faster and would silently diverge
// the day the standard library changed its rounding — image/color's YCbCr
// conversion is more precise in RGBA than in YCbCrToRGB, which is exactly the
// kind of difference that moves real pixels. What is removed here is the
// dispatch and the boxing, not the definition of the colour.
func grayFastPath(dst *image.Gray, src image.Image, bounds image.Rectangle) bool {
	width, height := bounds.Dx(), bounds.Dy()
	switch typed := src.(type) {
	case *image.Gray:
		// color.Gray.RGBA replicates Y into all three channels at 0x101 scale
		// with a=0xffff, and the luma weights sum to exactly 65536, so the
		// arithmetic is the identity on Y. A copy is therefore not an
		// approximation of the generic path; it is the same answer.
		for y := 0; y < height; y++ {
			start := typed.PixOffset(bounds.Min.X, bounds.Min.Y+y)
			copy(dst.Pix[y*dst.Stride:y*dst.Stride+width], typed.Pix[start:start+width])
		}
	case *image.Gray16:
		for y := 0; y < height; y++ {
			row := dst.Pix[y*dst.Stride : y*dst.Stride+width]
			start := typed.PixOffset(bounds.Min.X, bounds.Min.Y+y)
			for x := 0; x < width; x++ {
				pixel := typed.Pix[start+2*x : start+2*x+2 : start+2*x+2]
				sample := color.Gray16{Y: uint16(pixel[0])<<8 | uint16(pixel[1])}
				row[x] = grayFromPremultiplied(sample.RGBA())
			}
		}
	case *image.YCbCr:
		// What image/jpeg produces. Chroma is subsampled, so the plane offset
		// has to be resolved per pixel rather than strided like the luma plane.
		for y := 0; y < height; y++ {
			row := dst.Pix[y*dst.Stride : y*dst.Stride+width]
			sourceY := bounds.Min.Y + y
			luma := typed.YOffset(bounds.Min.X, sourceY)
			for x := 0; x < width; x++ {
				chroma := typed.COffset(bounds.Min.X+x, sourceY)
				sample := color.YCbCr{Y: typed.Y[luma+x], Cb: typed.Cb[chroma], Cr: typed.Cr[chroma]}
				row[x] = grayFromPremultiplied(sample.RGBA())
			}
		}
	case *image.RGBA:
		for y := 0; y < height; y++ {
			row := dst.Pix[y*dst.Stride : y*dst.Stride+width]
			start := typed.PixOffset(bounds.Min.X, bounds.Min.Y+y)
			for x := 0; x < width; x++ {
				pixel := typed.Pix[start+4*x : start+4*x+4 : start+4*x+4]
				sample := color.RGBA{R: pixel[0], G: pixel[1], B: pixel[2], A: pixel[3]}
				row[x] = grayFromPremultiplied(sample.RGBA())
			}
		}
	case *image.NRGBA:
		// The common PNG result.
		for y := 0; y < height; y++ {
			row := dst.Pix[y*dst.Stride : y*dst.Stride+width]
			start := typed.PixOffset(bounds.Min.X, bounds.Min.Y+y)
			for x := 0; x < width; x++ {
				pixel := typed.Pix[start+4*x : start+4*x+4 : start+4*x+4]
				sample := color.NRGBA{R: pixel[0], G: pixel[1], B: pixel[2], A: pixel[3]}
				row[x] = grayFromPremultiplied(sample.RGBA())
			}
		}
	case *image.RGBA64:
		for y := 0; y < height; y++ {
			row := dst.Pix[y*dst.Stride : y*dst.Stride+width]
			start := typed.PixOffset(bounds.Min.X, bounds.Min.Y+y)
			for x := 0; x < width; x++ {
				pixel := typed.Pix[start+8*x : start+8*x+8 : start+8*x+8]
				sample := color.RGBA64{
					R: uint16(pixel[0])<<8 | uint16(pixel[1]),
					G: uint16(pixel[2])<<8 | uint16(pixel[3]),
					B: uint16(pixel[4])<<8 | uint16(pixel[5]),
					A: uint16(pixel[6])<<8 | uint16(pixel[7]),
				}
				row[x] = grayFromPremultiplied(sample.RGBA())
			}
		}
	case *image.NRGBA64:
		for y := 0; y < height; y++ {
			row := dst.Pix[y*dst.Stride : y*dst.Stride+width]
			start := typed.PixOffset(bounds.Min.X, bounds.Min.Y+y)
			for x := 0; x < width; x++ {
				pixel := typed.Pix[start+8*x : start+8*x+8 : start+8*x+8]
				sample := color.NRGBA64{
					R: uint16(pixel[0])<<8 | uint16(pixel[1]),
					G: uint16(pixel[2])<<8 | uint16(pixel[3]),
					B: uint16(pixel[4])<<8 | uint16(pixel[5]),
					A: uint16(pixel[6])<<8 | uint16(pixel[7]),
				}
				row[x] = grayFromPremultiplied(sample.RGBA())
			}
		}
	case *image.Paletted:
		// A palette has at most 256 entries, so the whole conversion collapses
		// into a lookup table built once per image. Indices past the end of the
		// palette resolve to white here; image.Paletted.At would panic on them,
		// and a malformed index is not a reason to take the service down.
		if len(typed.Palette) == 0 || len(typed.Palette) > 256 {
			return false
		}
		var table [256]uint8
		for index := range table {
			table[index] = 0xff
		}
		for index, entry := range typed.Palette {
			table[index] = grayFromPremultiplied(entry.RGBA())
		}
		for y := 0; y < height; y++ {
			row := dst.Pix[y*dst.Stride : y*dst.Stride+width]
			start := typed.PixOffset(bounds.Min.X, bounds.Min.Y+y)
			for x := 0; x < width; x++ {
				row[x] = table[typed.Pix[start+x]]
			}
		}
	default:
		return false
	}
	return true
}

// scaledSize picks the limiting axis by cross-multiplication and rounds the
// other axis half-up. No floating point enters the geometry, so the result is
// identical on darwin/arm64 and linux/arm.
func scaledSize(srcW, srcH, screenW, screenH int) (int, int) {
	if int64(srcW)*int64(screenH) >= int64(screenW)*int64(srcH) {
		height := int((int64(srcH)*int64(screenW)*2 + int64(srcW)) / (int64(srcW) * 2))
		return screenW, clampInt(height, 1, screenH)
	}
	width := int((int64(srcW)*int64(screenH)*2 + int64(srcH)) / (int64(srcH) * 2))
	return clampInt(width, 1, screenW), screenH
}

func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// fitContain scales the whole source down to fit and centres it on a white
// canvas. The aspect ratio is preserved, so nothing is stretched.
func fitContain(src *image.Gray, screen ScreenCapabilities) *image.Gray {
	width, height := scaledSize(src.Rect.Dx(), src.Rect.Dy(), screen.Width, screen.Height)
	scaled := resampleGray(src, src.Rect, width, height)
	canvas := image.NewGray(image.Rect(0, 0, screen.Width, screen.Height))
	for index := range canvas.Pix {
		canvas.Pix[index] = 0xff
	}
	offsetX := (screen.Width - width) / 2
	offsetY := (screen.Height - height) / 2
	for y := 0; y < height; y++ {
		start := (y+offsetY)*canvas.Stride + offsetX
		copy(canvas.Pix[start:start+width], scaled.Pix[y*scaled.Stride:y*scaled.Stride+width])
	}
	return canvas
}

// fitCover crops the source to the panel's aspect ratio around its centre and
// scales that crop once to the exact panel size. Cropping in source coordinates
// avoids building an oversized intermediate image.
func fitCover(src *image.Gray, screen ScreenCapabilities) *image.Gray {
	srcW, srcH := src.Rect.Dx(), src.Rect.Dy()
	cropW, cropH := srcW, srcH
	if int64(srcW)*int64(screen.Height) >= int64(screen.Width)*int64(srcH) {
		cropW = clampInt(int((int64(srcH)*int64(screen.Width)*2+int64(screen.Height))/(int64(screen.Height)*2)), 1, srcW)
	} else {
		cropH = clampInt(int((int64(srcW)*int64(screen.Height)*2+int64(screen.Width))/(int64(screen.Width)*2)), 1, srcH)
	}
	// Floor division gives any surplus pixel to the right and bottom edges.
	left := src.Rect.Min.X + (srcW-cropW)/2
	top := src.Rect.Min.Y + (srcH-cropH)/2
	area := image.Rect(left, top, left+cropW, top+cropH)
	return resampleGray(src, area, screen.Width, screen.Height)
}

// resampleGray averages every source pixel that falls inside a destination
// pixel's box. Integer accumulation with half-up rounding keeps the output
// byte-identical across architectures, and an empty box clamps to a single
// source pixel so upscaling degenerates to pixel replication.
func resampleGray(src *image.Gray, area image.Rectangle, dstW, dstH int) *image.Gray {
	dst := image.NewGray(image.Rect(0, 0, dstW, dstH))
	srcW, srcH := area.Dx(), area.Dy()
	// The horizontal box of a destination column does not depend on the row, so
	// the two divisions per pixel become two divisions per column. On a
	// full-panel resample that is millions of integer divisions removed from the
	// synchronous request path; the boxes themselves are unchanged.
	lefts := make([]int, dstW)
	rights := make([]int, dstW)
	for x := 0; x < dstW; x++ {
		left := area.Min.X + x*srcW/dstW
		right := area.Min.X + (x+1)*srcW/dstW
		if right <= left {
			right = left + 1
		}
		if right > area.Max.X {
			right = area.Max.X
		}
		lefts[x], rights[x] = left, right
	}
	for y := 0; y < dstH; y++ {
		top := area.Min.Y + y*srcH/dstH
		bottom := area.Min.Y + (y+1)*srcH/dstH
		if bottom <= top {
			bottom = top + 1
		}
		if bottom > area.Max.Y {
			bottom = area.Max.Y
		}
		row := dst.Pix[y*dst.Stride : y*dst.Stride+dstW]
		for x := 0; x < dstW; x++ {
			left, right := lefts[x], rights[x]
			var sum uint64
			for sy := top; sy < bottom; sy++ {
				base := src.PixOffset(left, sy)
				for _, value := range src.Pix[base : base+right-left] {
					sum += uint64(value)
				}
			}
			count := uint64((right - left) * (bottom - top))
			row[x] = uint8((2*sum + count) / (2 * count))
		}
	}
	return dst
}
