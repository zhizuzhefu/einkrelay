package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ErrDisplayBackend is the opaque failure of the external display backend. The
// underlying exec error can carry device paths and command output, so it never
// travels any further than this file.
var ErrDisplayBackend = errors.New("the display backend failed")

const (
	// backendProbeTimeout bounds the one-off capability probe run at startup.
	backendProbeTimeout = 5 * time.Second
	// maxBackendVersionLength truncates whatever the backend prints, so an
	// unexpectedly chatty binary cannot inflate every status response.
	maxBackendVersionLength = 64
)

// PanelWriter is the seam between a committed candidate file and the physical
// panel. Everything above it operates on already-validated bytes; everything
// below it is device-specific and can only be proven on hardware.
type PanelWriter interface {
	Show(ctx context.Context, path string) error
	Status(ctx context.Context) BackendStatus
}

// FBInkPanel drives the external FBInk executable. FBInk is invoked strictly as
// a separate process: it is never linked in, which is what keeps this build
// CGO-free and keeps FBInk's own licence separable from this program.
type FBInkPanel struct {
	Path string

	mu      sync.Mutex
	version *string
	failed  bool
}

// Show hands one already-validated, already-fsynced candidate file to FBInk.
// The panel is never cleared first: a failed call has to leave the last
// successful screen exactly where it is.
func (p *FBInkPanel) Show(ctx context.Context, path string) error {
	command := exec.CommandContext(ctx, p.Path, "-q", "-f", "-g", "file="+path)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	err := command.Run()
	p.mu.Lock()
	p.failed = err != nil
	p.mu.Unlock()
	if err != nil {
		// A cancelled or timed-out transaction is reported as such so the HTTP
		// layer can map it to the frozen 504 rather than to a generic failure.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrDisplayBackend
	}
	return nil
}

// Probe records the backend version once, parsed from the `--help` banner —
// the only probe a real FBInk CLI honours (upstream has no `--version` flag).
// Running it per status request would fork a process on every poll, which is
// not something a ~490MB device should do on a path that exists to be polled.
func (p *FBInkPanel) Probe(ctx context.Context) {
	scoped, cancel := context.WithTimeout(ctx, backendProbeTimeout)
	defer cancel()
	output, err := exec.CommandContext(scoped, p.Path, "--help").Output()
	if err != nil {
		return
	}
	value := sanitizeBackendVersion(string(output))
	if value == "" {
		return
	}
	p.mu.Lock()
	p.version = &value
	p.mu.Unlock()
}

// sanitizeBackendVersion keeps the first non-blank line — a real FBInk help
// banner leads with an empty line before "FBInk vX.Y.Z for Kindle ..." — drops
// anything outside printable ASCII and truncates the rest. Whatever the device
// binary emits, only a short predictable string can reach a status response.
func sanitizeBackendVersion(raw string) string {
	line := ""
	for _, candidate := range strings.Split(raw, "\n") {
		if strings.TrimSpace(candidate) != "" {
			line = candidate
			break
		}
	}
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, strings.TrimSpace(line))
	cleaned = strings.TrimSpace(cleaned)
	if len(cleaned) > maxBackendVersionLength {
		cleaned = cleaned[:maxBackendVersionLength]
	}
	return cleaned
}

// Status reports what can be known without forking: whether the executable is
// still present and executable, and whether the last display attempt failed.
func (p *FBInkPanel) Status(context.Context) BackendStatus {
	p.mu.Lock()
	version, failed := p.version, p.failed
	p.mu.Unlock()
	state := "ready"
	info, err := os.Stat(p.Path)
	switch {
	case err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0:
		state = "unavailable"
	case failed:
		state = "error"
	}
	return BackendStatus{Name: "fbink", State: state, Version: version}
}

var _ PanelWriter = (*FBInkPanel)(nil)

// RenderingBackend is the production DisplayBackend: it rasterises a request,
// then runs the durable display transaction. Rendering and persistence live
// behind the same port so that the HTTP layer only ever sees the frozen
// sentinels, and so that no request can reach the panel without having survived
// validation first.
//
// A RenderingBackend is not safe for concurrent use. It does not need to be:
// the handler's shared display latch admits exactly one transaction at a time,
// which is also what makes the FontLibrary's face cache safe here.
type RenderingBackend struct {
	Store  *DisplayStore
	Panel  PanelWriter
	Limits ImageLimits
	Style  MarkdownStyle
	// Fonts is nil when the pinned fonts did not verify. FontErr then carries
	// the reason, and Markdown rendering fails closed rather than drawing a
	// page of notdef boxes that would look like a successful screen.
	Fonts   *FontLibrary
	FontErr error
}

func (b *RenderingBackend) DisplayImage(ctx context.Context, mediaType, fit string, body []byte, screen ScreenCapabilities) (DisplayResult, error) {
	payload, err := RenderImage(mediaType, fit, body, screen, b.Limits)
	if err != nil {
		// Rejected before any candidate file exists, so nothing was written, the
		// panel was not touched and current/previous are unchanged.
		return DisplayResult{}, err
	}
	return b.Store.Commit(ctx, payload, screen, b.Panel.Show)
}

func (b *RenderingBackend) DisplayMarkdown(ctx context.Context, body []byte, screen ScreenCapabilities, style MarkdownStyle) (DisplayResult, error) {
	if b.Fonts == nil {
		if b.FontErr != nil {
			return DisplayResult{}, b.FontErr
		}
		return DisplayResult{}, ErrFontMissing
	}
	payload, err := RenderMarkdown(body, screen, b.Fonts, style)
	if err != nil {
		return DisplayResult{}, err
	}
	return b.Store.Commit(ctx, payload, screen, b.Panel.Show)
}

func (b *RenderingBackend) Status(ctx context.Context) BackendStatus {
	return b.Panel.Status(ctx)
}

var _ DisplayBackend = (*RenderingBackend)(nil)

// RestoreLastScreen re-displays the newest screen that still validates. It only
// ever hands the panel bytes that have been re-verified against the probed
// geometry, and it neither rotates nor rewrites anything: a restore is a read.
func RestoreLastScreen(ctx context.Context, store *DisplayStore, panel PanelWriter, screen ScreenCapabilities) (RecoveredScreen, error) {
	recovered, err := store.Recover(screen)
	if err != nil {
		return RecoveredScreen{}, err
	}
	if err := panel.Show(ctx, recovered.Path); err != nil {
		return RecoveredScreen{}, err
	}
	return recovered, nil
}

// restoreLastScreenAtStartup applies the activity record's decision. Every
// failure leaves the panel exactly as it was found and becomes a sanitized entry
// in /v1/status, because showing unvalidated data or blanking the screen would
// both be worse than showing whatever the device was already showing.
func restoreLastScreenAtStartup(ctx context.Context, backend *RenderingBackend, probe ScreenProbe, state *StateStore, log io.Writer) {
	capabilities, err := probe.Probe(ctx)
	if err != nil || capabilities.Width < 1 || capabilities.Height < 1 {
		state.RecordError("display_failed", "the panel geometry could not be probed")
		fmt.Fprintln(log, "startup: the panel geometry could not be probed; the screen was left untouched")
		return
	}
	recovered, err := RestoreLastScreen(ctx, backend.Store, backend.Panel, capabilities)
	if err == nil {
		state.Commit(DisplayResult{SHA256: recovered.SHA256, DisplayedAt: time.Now().UTC()})
		return
	}
	// First boot: nothing verifiable to restore. A white panel looks like a
	// dead service, so the built-in help page is shown through the normal
	// transaction and becomes the first committed screen. It needs the font
	// library; without it the panel is still left untouched. The page uses its
	// own enlarged style: DefaultMarkdownStyle is sized for dense user content,
	// while the help page is short instructional text that should be legible at
	// arm's length.
	if backend.Fonts != nil {
		payload, renderErr := RenderMarkdown([]byte(defaultHelpPageMarkdown), capabilities, backend.Fonts, defaultHelpPageStyle())
		if renderErr == nil {
			result, showErr := backend.Store.Commit(ctx, payload, capabilities, backend.Panel.Show)
			if showErr == nil {
				state.Commit(result)
				fmt.Fprintln(log, "startup: no saved screen; displayed the built-in help page")
				return
			}
		}
	}
	state.RecordError("persistence_failed", "no verifiable screen was available to restore")
	fmt.Fprintln(log, "startup: no verifiable screen was available to restore; the screen was left untouched")
}

// loadRuntimeFonts verifies every pinned font before the first render rather
// than at the first request, so a missing or tampered face is visible in
// /v1/status from the moment the service starts.
func loadRuntimeFonts(getenv func(string) string) (*FontLibrary, error) {
	cfg, err := LoadFontConfigFromEnv(getenv)
	if err != nil {
		return nil, ErrFontManifest
	}
	manifest, err := LoadFontManifest(cfg.ManifestPath)
	if err != nil {
		return nil, err
	}
	return LoadFontLibrary(manifest, cfg.Dir)
}

// fontStatusMessage is the stable, sanitized wording used both on stderr and in
// /v1/status. It names no path, no URL and no digest.
func fontStatusMessage(err error) string {
	switch {
	case errors.Is(err, ErrFontDigest):
		return "the installed fonts do not match the pinned manifest"
	case errors.Is(err, ErrFontMissing):
		return "the pinned fonts are not installed"
	default:
		return "the font manifest is unusable"
	}
}
