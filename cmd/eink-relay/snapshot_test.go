package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// fakeFramebuffer builds a file shaped like /dev/fb0: rows padded to a stride
// wider than the panel, and enough of them for several stacked buffers. The
// visible picture is the top-left width x height window, and everything else is
// filled with a marker the test can detect if it ever leaks into the capture.
func fakeFramebuffer(t *testing.T, width, height, stride, rows int, shade func(x, y int) uint8) string {
	t.Helper()
	raw := make([]byte, stride*rows)
	for index := range raw {
		raw[index] = 0x7f // the marker: neither black nor white
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			raw[y*stride+x] = shade(x, y)
		}
	}
	path := filepath.Join(t.TempDir(), "fb0")
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func sysfsValue(t *testing.T, name, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(value+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestSnapshot(t *testing.T, framebuffer string, width, height, stride int, panel PanelWriter) *FramebufferSnapshot {
	t.Helper()
	return &FramebufferSnapshot{
		Path:        filepath.Join(t.TempDir(), snapshotName),
		Framebuffer: framebuffer,
		Stride:      sysfsValue(t, "stride", strconv.Itoa(stride)),
		Depth:       sysfsValue(t, "bits_per_pixel", "8"),
		Probe:       &FakeScreen{Capabilities: ScreenCapabilities{Width: width, Height: height}},
		Panel:       panel,
	}
}

// TestSnapshotCapturesOnlyTheVisibleWindow is the whole reason the capture
// cannot be a straight copy of the device node. The framebuffer is padded wider
// than the panel and holds several stacked buffers; copying it verbatim would
// restore a frame that is mostly somebody else's buffer.
func TestSnapshotCapturesOnlyTheVisibleWindow(t *testing.T) {
	const width, height, stride, rows = 1072, 1448, 1088, 6144
	shade := func(x, y int) uint8 { return uint8((x*3 + y*5) % 251) }
	framebuffer := fakeFramebuffer(t, width, height, stride, rows, shade)
	panel := &recordingPanel{}
	snapshot := newTestSnapshot(t, framebuffer, width, height, stride, panel)

	if err := snapshot.Capture(context.Background()); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	payload, err := os.ReadFile(snapshot.Path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("the captured frame is not a PNG: %v", err)
	}
	frame, ok := decoded.(*image.Gray)
	if !ok {
		t.Fatalf("the captured frame is not 8-bit grayscale: %T", decoded)
	}
	if frame.Rect.Dx() != width || frame.Rect.Dy() != height {
		t.Fatalf("captured %dx%d, want the visible panel %dx%d", frame.Rect.Dx(), frame.Rect.Dy(), width, height)
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if got, want := frame.Pix[y*frame.Stride+x], shade(x, y); got != want {
				t.Fatalf("pixel (%d,%d) = %d, want %d", x, y, got, want)
			}
		}
	}
	// The capture must be a frame the display pipeline itself would accept.
	if err := validateFullScreenPNG(payload, ScreenCapabilities{Width: width, Height: height}); err != nil {
		t.Fatalf("the captured frame is not a valid full-screen frame: %v", err)
	}
}

func TestSnapshotRestoreHandsTheCapturedFrameToThePanel(t *testing.T) {
	const width, height, stride, rows = 32, 20, 40, 100
	framebuffer := fakeFramebuffer(t, width, height, stride, rows, func(x, y int) uint8 { return uint8(x + y) })
	panel := &recordingPanel{}
	snapshot := newTestSnapshot(t, framebuffer, width, height, stride, panel)

	if err := snapshot.Capture(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(panel.shown) != 1 || panel.shown[0] != snapshot.Path {
		t.Fatalf("panel received %v, want exactly the captured frame", panel.shown)
	}
}

// TestSnapshotRestoreWithoutACaptureFails keeps the recovery honest. Reporting
// success without having put anything back is what let the old design claim a
// device had been restored while the reader was looking at a frozen screen.
func TestSnapshotRestoreWithoutACaptureFails(t *testing.T) {
	panel := &recordingPanel{}
	snapshot := &FramebufferSnapshot{Path: filepath.Join(t.TempDir(), snapshotName), Panel: panel}
	if err := snapshot.Restore(context.Background()); !errors.Is(err, ErrSnapshot) {
		t.Fatalf("Restore error = %v, want ErrSnapshot", err)
	}
	if len(panel.shown) != 0 {
		t.Fatalf("the panel was touched without a capture: %v", panel.shown)
	}
}

func TestSnapshotRestoreReportsAPanelFailure(t *testing.T) {
	const width, height, stride, rows = 16, 8, 24, 40
	framebuffer := fakeFramebuffer(t, width, height, stride, rows, func(x, y int) uint8 { return 0x10 })
	panel := &recordingPanel{err: errors.New("fbink failed")}
	snapshot := newTestSnapshot(t, framebuffer, width, height, stride, panel)
	if err := snapshot.Capture(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Restore(context.Background()); !errors.Is(err, ErrSnapshot) {
		t.Fatalf("Restore error = %v, want ErrSnapshot", err)
	}
}

// TestSnapshotCaptureFailsClosedOnAnUnsupportedFramebuffer covers every way the
// device could be shaped differently from the one this product targets.
// Guessing at a layout would restore garbage, which is worse than refusing to
// enter exclusive mode at all.
func TestSnapshotCaptureFailsClosedOnAnUnsupportedFramebuffer(t *testing.T) {
	const width, height, stride, rows = 32, 20, 40, 100
	framebuffer := fakeFramebuffer(t, width, height, stride, rows, func(x, y int) uint8 { return 1 })

	t.Run("non-8-bit depth", func(t *testing.T) {
		snapshot := newTestSnapshot(t, framebuffer, width, height, stride, &recordingPanel{})
		snapshot.Depth = sysfsValue(t, "bits_per_pixel", "16")
		if err := snapshot.Capture(context.Background()); !errors.Is(err, ErrSnapshot) {
			t.Fatalf("a 16-bit framebuffer was accepted: %v", err)
		}
	})

	t.Run("stride narrower than the panel", func(t *testing.T) {
		snapshot := newTestSnapshot(t, framebuffer, width, height, stride, &recordingPanel{})
		snapshot.Stride = sysfsValue(t, "stride", "8")
		if err := snapshot.Capture(context.Background()); !errors.Is(err, ErrSnapshot) {
			t.Fatalf("a stride narrower than the panel was accepted: %v", err)
		}
	})

	t.Run("framebuffer shorter than the panel", func(t *testing.T) {
		short := fakeFramebuffer(t, width, 2, stride, 2, func(x, y int) uint8 { return 1 })
		snapshot := newTestSnapshot(t, short, width, height, stride, &recordingPanel{})
		if err := snapshot.Capture(context.Background()); !errors.Is(err, ErrSnapshot) {
			t.Fatalf("a truncated framebuffer was accepted: %v", err)
		}
	})

	t.Run("unreadable nodes", func(t *testing.T) {
		snapshot := newTestSnapshot(t, filepath.Join(t.TempDir(), "absent"), width, height, stride, &recordingPanel{})
		if err := snapshot.Capture(context.Background()); !errors.Is(err, ErrSnapshot) {
			t.Fatalf("a missing framebuffer was accepted: %v", err)
		}
	})

	t.Run("unprobeable screen", func(t *testing.T) {
		snapshot := newTestSnapshot(t, framebuffer, width, height, stride, &recordingPanel{})
		snapshot.Probe = &FakeScreen{Err: errors.New("no geometry")}
		if err := snapshot.Capture(context.Background()); !errors.Is(err, ErrSnapshot) {
			t.Fatalf("an unprobeable screen was accepted: %v", err)
		}
	})
}
