package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrSnapshot is the opaque failure of a panel snapshot. Like every other
// device-facing sentinel it carries no path and no device detail.
var ErrSnapshot = errors.New("panel snapshot failed")

const (
	defaultFramebufferPath = "/dev/fb0"
	defaultStridePath      = "/sys/class/graphics/fb0/stride"
	defaultDepthPath       = "/sys/class/graphics/fb0/bits_per_pixel"
	// snapshotName is the frame the panel is restored to on the way out. It
	// lives beside the display store's own frames and is deliberately not one
	// of them: it is not a screen this service produced and must never be
	// promoted into current/previous or reported as the last successful frame.
	snapshotName = "panel-snapshot.png"
)

// PanelSnapshot preserves whatever the native interface had on the panel before
// exclusive mode took it, and puts those exact pixels back afterwards.
//
// This is the recovery. The obvious alternative — ask the native interface to
// redraw itself — does not work on this firmware and cannot be made to. The
// interface is an event-driven Xorg/awesome/blanket stack that repaints in
// response to things it knows about; a framebuffer write is not one of them, so
// it never learns its pixels are gone. Every published lipc verb that looked
// like it might force a redraw was tried against a covered panel and compared
// frame by frame: pillow's disableEnablePillow, displayChrome and
// interrogatePillow, appmgrd's startdefault, and powerd's outOfScreenSaver
// event all left the panel untouched. powerd's wakeUp does repaint — but only
// while the device is actually asleep; awake it answers lipcErrNoSuchProperty,
// which is exactly the state an exit runs in.
//
// Restoring the pixels ourselves needs no cooperation from any of that. It is
// also more correct than a redraw would be: because the touch stream is held
// exclusively while the panel is covered, the interface underneath never
// advanced, so the frame captured on the way in still describes its true state.
// The next real interaction repaints it naturally and refreshes anything that
// has since gone stale, such as the clock.
// The stored file is not a cache. It is the durable record of a debt: while it
// exists, this service owes the native interface a frame it covered up. That
// framing is what makes the whole thing correct across a crash. A Guardian that
// is killed while the panel is covered leaves the file behind; the next one
// sees the debt still outstanding, does NOT capture over it — the panel is
// showing our content by then, and capturing would replace the frame we owe
// with the one we owe it *for* — and pays it back on the next exit. Symmetric-
// ally, a service that never covered anything owes nothing, so its exit has
// nothing to restore and must not report failure.
type PanelSnapshot interface {
	// Owed reports whether a frame is still owed back to the native interface.
	Owed() bool
	Capture(ctx context.Context) error
	Restore(ctx context.Context) error
	// Discard settles the debt. It runs only after a restore has succeeded.
	Discard() error
}

// FramebufferSnapshot reads the panel straight out of /dev/fb0.
type FramebufferSnapshot struct {
	// Path is where the captured frame is stored between capture and restore.
	Path string
	// Framebuffer, Stride and Depth locate the kernel nodes; empty selects the
	// production defaults. Tests point them at ordinary files.
	Framebuffer string
	Stride      string
	Depth       string
	Probe       ScreenProbe
	Panel       PanelWriter
}

func (f *FramebufferSnapshot) framebuffer() string {
	if f.Framebuffer != "" {
		return f.Framebuffer
	}
	return defaultFramebufferPath
}

func (f *FramebufferSnapshot) stridePath() string {
	if f.Stride != "" {
		return f.Stride
	}
	return defaultStridePath
}

func (f *FramebufferSnapshot) depthPath() string {
	if f.Depth != "" {
		return f.Depth
	}
	return defaultDepthPath
}

func readSysfsInt(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, ErrSnapshot
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || value < 1 {
		return 0, ErrSnapshot
	}
	return value, nil
}

// Owed reports whether a captured frame is still waiting to be handed back.
func (f *FramebufferSnapshot) Owed() bool {
	if f.Path == "" {
		return false
	}
	info, err := os.Lstat(f.Path)
	return err == nil && info.Mode().IsRegular()
}

// Discard settles the debt once the frame is back on the panel.
func (f *FramebufferSnapshot) Discard() error {
	if f.Path == "" {
		return nil
	}
	if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
		return ErrSnapshot
	}
	return syncDir(filepath.Dir(f.Path))
}

// Capture copies the visible region out of the framebuffer. The framebuffer is
// wider than the panel (the stride is padded) and taller (it holds several
// stacked buffers), so only the top-left width x height window is the picture a
// reader can actually see.
func (f *FramebufferSnapshot) Capture(ctx context.Context) error {
	if f.Path == "" || f.Probe == nil {
		return ErrSnapshot
	}
	screen, err := f.Probe.Probe(ctx)
	if err != nil || screen.Width < 1 || screen.Height < 1 {
		return ErrSnapshot
	}
	depth, err := readSysfsInt(f.depthPath())
	if err != nil {
		return err
	}
	if depth != 8 {
		// Only the 8-bit grayscale panel this product targets is understood.
		// Guessing at another layout would restore garbage, which is worse than
		// restoring nothing.
		return ErrSnapshot
	}
	stride, err := readSysfsInt(f.stridePath())
	if err != nil {
		return err
	}
	if stride < screen.Width {
		return ErrSnapshot
	}
	// Read row by row rather than slurping the node. The framebuffer holds
	// several stacked buffers — 6.7MB on a PW4 — of which the visible panel is
	// the top 1.5MB, and this runs on a device with about 490MB of RAM.
	device, err := os.Open(f.framebuffer())
	if err != nil {
		return ErrSnapshot
	}
	defer device.Close()
	frame := image.NewGray(image.Rect(0, 0, screen.Width, screen.Height))
	row := make([]byte, stride)
	for y := 0; y < screen.Height; y++ {
		if _, err := io.ReadFull(device, row); err != nil {
			return ErrSnapshot
		}
		copy(frame.Pix[y*frame.Stride:y*frame.Stride+screen.Width], row[:screen.Width])
	}
	payload, err := encodeSnapshotPNG(frame)
	if err != nil {
		return ErrSnapshot
	}
	return writeDurableFile(f.Path, payload)
}

// Restore hands the captured frame back to the panel. A missing capture is a
// failure rather than a silent no-op: the caller asked for the screen back and
// did not get it, and /v1/status is where that has to show up.
func (f *FramebufferSnapshot) Restore(ctx context.Context) error {
	if f.Path == "" || f.Panel == nil {
		return ErrSnapshot
	}
	info, err := os.Lstat(f.Path)
	if err != nil || !info.Mode().IsRegular() {
		return ErrSnapshot
	}
	if err := f.Panel.Show(ctx, f.Path); err != nil {
		return ErrSnapshot
	}
	return nil
}

var _ PanelSnapshot = (*FramebufferSnapshot)(nil)

func encodeSnapshotPNG(frame *image.Gray) ([]byte, error) {
	var buffer bytes.Buffer
	if err := encodeScreenPNG(&buffer, frame); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// writeDurableFile replaces path atomically. The snapshot has to survive the
// service process being restarted underneath the Guardian, which is the whole
// reason it is a file rather than a buffer in memory.
func writeDurableFile(path string, payload []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), "panel-snapshot-*.tmp")
	if err != nil {
		return ErrSnapshot
	}
	name := file.Name()
	abandon := func() error {
		_ = file.Close()
		_ = os.Remove(name)
		return ErrSnapshot
	}
	if _, err := file.Write(payload); err != nil {
		return abandon()
	}
	if err := file.Chmod(0600); err != nil {
		return abandon()
	}
	if err := file.Sync(); err != nil {
		return abandon()
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return ErrSnapshot
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return ErrSnapshot
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return ErrSnapshot
	}
	return nil
}
