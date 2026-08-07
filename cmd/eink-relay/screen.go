package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
)

// defaultModesPath carries the framebuffer's *visible* video modes. Reading it
// at runtime is what keeps the service free of a hard-coded PW4 resolution: the
// same binary adapts to whatever panel it is started on.
//
// It deliberately replaces the older `virtual_size` probe. On the PW4,
// virtual_size reports 1088x6144 — the stride-padded width of the virtual
// framebuffer and enough rows for several stacked buffers — while the panel
// that a reader can actually see is 1072x1448. Treating the virtual geometry as
// the panel meant every frame was laid out on a canvas 4.2x taller than the
// screen: Markdown lost everything below the first quarter of the page, and an
// image centred with fit=contain landed entirely below the visible rows, so a
// perfectly successful request displayed nothing but white.
const defaultModesPath = "/sys/class/graphics/fb0/modes"

// maxProbedDimension is a sanity ceiling. A framebuffer node that reports
// something larger is treated as unreadable rather than trusted, so a corrupt
// sysfs value cannot drive an absurd allocation later in the pipeline.
const maxProbedDimension = 8192

// SysfsScreenProbe reads the runtime screen capabilities from the framebuffer.
// Path is only overridden by tests; production uses the kernel node.
type SysfsScreenProbe struct {
	Path string
}

func (p *SysfsScreenProbe) Probe(context.Context) (ScreenCapabilities, error) {
	path := p.Path
	if path == "" {
		path = defaultModesPath
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ScreenCapabilities{}, errors.New("screen probe failed")
	}
	return parseVideoModes(string(raw))
}

// parseVideoModes accepts the fbdev mode list written by the kernel. Each line
// is `<name>:<xres>x<yres><p|i>-<refresh>`, for example `U:1072x1448p-0`, and
// the first line is the mode in force. Later lines describe the alternatives
// the panel could be switched to — on the PW4 the second entry is the same
// geometry transposed — so only the first is read.
//
// The probe fails closed when the node is missing or unparseable rather than
// falling back to virtual_size: silently resuming the geometry that produced
// invisible frames would be worse than refusing to display.
func parseVideoModes(raw string) (ScreenCapabilities, error) {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return parseVideoMode(line)
	}
	return ScreenCapabilities{}, errors.New("screen probe failed")
}

func parseVideoMode(line string) (ScreenCapabilities, error) {
	fail := func() (ScreenCapabilities, error) {
		return ScreenCapabilities{}, errors.New("screen probe failed")
	}
	// Drop the leading `<name>:` if present; a mode line without one is still
	// readable, so the colon is not required.
	if index := strings.IndexByte(line, ':'); index >= 0 {
		line = line[index+1:]
	}
	// Drop the trailing `-<refresh>`.
	if index := strings.IndexByte(line, '-'); index >= 0 {
		line = line[:index]
	}
	// Drop the trailing progressive/interlaced marker.
	line = strings.TrimRight(line, "pi")
	width, height, found := strings.Cut(line, "x")
	if !found {
		return fail()
	}
	parsedWidth, widthErr := strconv.Atoi(strings.TrimSpace(width))
	parsedHeight, heightErr := strconv.Atoi(strings.TrimSpace(height))
	if widthErr != nil || heightErr != nil {
		return fail()
	}
	if parsedWidth < 1 || parsedHeight < 1 || parsedWidth > maxProbedDimension || parsedHeight > maxProbedDimension {
		return fail()
	}
	return ScreenCapabilities{Width: parsedWidth, Height: parsedHeight}, nil
}

var _ ScreenProbe = (*SysfsScreenProbe)(nil)
