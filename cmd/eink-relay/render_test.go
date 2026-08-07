package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recordingPanel stands in for the external FBInk process. It records the exact
// paths it was handed, which is how the tests prove that only a validated
// candidate ever reaches the panel.
type recordingPanel struct {
	mu    sync.Mutex
	shown []string
	err   error
	state string
}

func (p *recordingPanel) Show(_ context.Context, path string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.shown = append(p.shown, path)
	return p.err
}

func (p *recordingPanel) Status(context.Context) BackendStatus {
	state := p.state
	if state == "" {
		state = "ready"
	}
	return BackendStatus{Name: "recording-panel", State: state}
}

func (p *recordingPanel) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.shown)
}

func newRenderingBackend(t *testing.T) (*RenderingBackend, *recordingPanel, string) {
	t.Helper()
	directory := t.TempDir()
	panel := &recordingPanel{}
	backend := &RenderingBackend{
		Store:  NewDisplayStore(directory),
		Panel:  panel,
		Limits: DefaultImageLimits(),
		Style:  DefaultMarkdownStyle(),
	}
	return backend, panel, directory
}

func TestRenderingBackendCommitsOnlyAfterThePanelSucceeds(t *testing.T) {
	screen := ScreenCapabilities{Width: 40, Height: 30}
	backend, panel, directory := newRenderingBackend(t)
	body := grayFixture(t, 20, 10, func(int, int) uint8 { return 0x20 })

	result, err := backend.DisplayImage(context.Background(), "image/png", "contain", body, screen)
	if err != nil {
		t.Fatalf("the image pipeline failed: %v", err)
	}
	if len(result.SHA256) != 64 || result.DisplayedAt.IsZero() {
		t.Fatalf("the committed result is incomplete: %+v", result)
	}
	if panel.calls() != 1 {
		t.Fatalf("the panel was called %d times", panel.calls())
	}
	if filepath.Base(panel.shown[0]) == currentImageName {
		t.Fatal("the panel was handed the committed path instead of the candidate")
	}
	committed := readFile(t, filepath.Join(directory, currentImageName))
	if err := validateFullScreenPNG(committed, screen); err != nil {
		t.Fatalf("current.png is not a verifiable full-screen frame: %v", err)
	}
	assertNoCandidates(t, directory)
}

func TestRenderingBackendKeepsTheLastScreenWhenThePanelFails(t *testing.T) {
	screen := ScreenCapabilities{Width: 40, Height: 30}
	backend, panel, directory := newRenderingBackend(t)
	good := grayFixture(t, 20, 10, func(int, int) uint8 { return 0x20 })
	if _, err := backend.DisplayImage(context.Background(), "image/png", "contain", good, screen); err != nil {
		t.Fatal(err)
	}
	committed := readFile(t, filepath.Join(directory, currentImageName))

	panel.err = ErrDisplayBackend
	later := grayFixture(t, 20, 10, func(int, int) uint8 { return 0xd0 })
	if _, err := backend.DisplayImage(context.Background(), "image/png", "contain", later, screen); !errors.Is(err, ErrDisplayBackend) {
		t.Fatalf("a failed panel call was not reported: %v", err)
	}
	if string(readFile(t, filepath.Join(directory, currentImageName))) != string(committed) {
		t.Fatal("a failed display promoted its candidate")
	}
	assertNoCandidates(t, directory)
}

func TestRenderingBackendRejectionsNeverReachThePanel(t *testing.T) {
	screen := ScreenCapabilities{Width: 40, Height: 30}
	cases := []struct {
		name      string
		mediaType string
		body      []byte
		want      error
	}{
		{name: "edge over 8192", mediaType: "image/png", body: pngHeaderOnly(9000, 10, 8, 0, 0), want: ErrImageDimensions},
		{name: "interlaced png", mediaType: "image/png", body: pngHeaderOnly(16, 16, 8, 0, 1), want: ErrImageDimensions},
		{name: "progressive jpeg", mediaType: "image/jpeg", body: jpegHeaderOnly(0xc2, 16, 16, 3), want: ErrImageDimensions},
		{name: "cmyk jpeg", mediaType: "image/jpeg", body: jpegHeaderOnly(0xc0, 16, 16, 4), want: ErrImageDimensions},
		{name: "ycck jpeg declared after the frame header", mediaType: "image/jpeg", body: jpegWithAdobeTransform(2, true, 16, 16, 3), want: ErrImageDimensions},
		{name: "not an image", mediaType: "image/png", body: []byte("definitely not a png"), want: ErrDecodeFailed},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			backend, panel, directory := newRenderingBackend(t)
			if _, err := backend.DisplayImage(context.Background(), test.mediaType, "contain", test.body, screen); !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
			if panel.calls() != 0 {
				t.Fatal("a rejected request reached the panel")
			}
			if _, err := os.Stat(filepath.Join(directory, currentImageName)); err == nil {
				t.Fatal("a rejected request was committed")
			}
			assertNoCandidates(t, directory)
		})
	}
}

func TestRenderingBackendMarkdownFailsClosedWithoutVerifiedFonts(t *testing.T) {
	screen := ScreenCapabilities{Width: 200, Height: 150}
	for name, configured := range map[string]error{
		"not installed":     ErrFontMissing,
		"digest mismatch":   ErrFontDigest,
		"unusable manifest": ErrFontManifest,
	} {
		t.Run(name, func(t *testing.T) {
			backend, panel, directory := newRenderingBackend(t)
			backend.FontErr = configured
			if _, err := backend.DisplayMarkdown(context.Background(), []byte("# Title"), screen, DefaultMarkdownStyle()); !errors.Is(err, configured) {
				t.Fatalf("got %v, want %v", err, configured)
			}
			if panel.calls() != 0 {
				t.Fatal("an unrenderable request reached the panel")
			}
			if _, err := os.Stat(filepath.Join(directory, currentImageName)); err == nil {
				t.Fatal("an unrenderable request was committed")
			}
		})
	}
}

// TestSuccessfulImageDisplayDoesNotClearThePersistentFontCondition is the image
// path's half of the font defect, and the worse half. DisplayImage never
// consults Fonts or FontErr — correctly so, since a bitmap needs no faces — so a
// device whose pinned fonts do not verify keeps serving images indefinitely. If
// each of those successes clears the recorded condition, /v1/status reads clean
// forever while every Markdown request keeps failing, and nothing points at the
// repair that is actually needed.
//
// The state store is driven directly against the DisplayResult the backend
// returns, which is the same seam the HTTP layer commits on (Handler.displayBody
// calls state.Commit(result)); wiring a store into RenderingBackend would be new
// production plumbing that this contract does not ask for.
func TestSuccessfulImageDisplayDoesNotClearThePersistentFontCondition(t *testing.T) {
	screen := ScreenCapabilities{Width: 40, Height: 30}
	backend, panel, directory := newRenderingBackend(t)
	// Exactly the startup shape: the pinned fonts did not verify, so no library
	// was loaded and the reason was kept.
	backend.Fonts = nil
	backend.FontErr = ErrFontDigest

	state := NewStateStore(true)
	message := fontStatusMessage(backend.FontErr)
	state.SetPersistentError("display_failed", message)

	body := grayFixture(t, 20, 10, func(int, int) uint8 { return 0x20 })
	result, err := backend.DisplayImage(context.Background(), "image/png", "contain", body, screen)
	if err != nil {
		t.Fatalf("the image path refused to serve although it needs no fonts: %v", err)
	}
	if panel.calls() != 1 {
		t.Fatalf("the panel was called %d times", panel.calls())
	}
	if err := validateFullScreenPNG(readFile(t, filepath.Join(directory, currentImageName)), screen); err != nil {
		t.Fatalf("the committed frame is not verifiable: %v", err)
	}

	state.Commit(result)

	snapshot := state.Snapshot("test-version", false, BackendStatus{})
	if snapshot.Current == nil || snapshot.Current.SHA256 != result.SHA256 {
		t.Fatalf("the successful image display was not recorded: %+v", snapshot.Current)
	}
	if snapshot.LastError == nil {
		t.Fatal("a successful image display cleared the font condition; the device would serve images forever while markdown stays broken and /v1/status reads clean")
	}
	if snapshot.LastError.Code != "display_failed" || snapshot.LastError.Message != message {
		t.Fatalf("the reported font condition changed: %+v", snapshot.LastError)
	}

	// The condition is still true of the backend as well, so markdown has to go
	// on failing closed rather than drawing a page of notdef boxes.
	if _, err := backend.DisplayMarkdown(context.Background(), []byte("# Title"), screen, DefaultMarkdownStyle()); !errors.Is(err, ErrFontDigest) {
		t.Fatalf("markdown stopped failing closed after a successful image display: %v", err)
	}
	if panel.calls() != 1 {
		t.Fatalf("an unrenderable markdown request reached the panel; calls=%d", panel.calls())
	}

	// A second successful image display must not wear the condition down either.
	later := grayFixture(t, 20, 10, func(int, int) uint8 { return 0xd0 })
	second, err := backend.DisplayImage(context.Background(), "image/png", "contain", later, screen)
	if err != nil {
		t.Fatalf("the second image display failed: %v", err)
	}
	state.Commit(second)
	if repeated := state.Snapshot("test-version", false, BackendStatus{}); repeated.LastError == nil || repeated.LastError.Code != "display_failed" || repeated.LastError.Message != message {
		t.Fatalf("the font condition did not survive repeated successful displays: %+v", repeated.LastError)
	}
}

func TestRenderingBackendMarkdownCommitsWithLoadedFonts(t *testing.T) {
	screen := ScreenCapabilities{Width: 300, Height: 220}
	backend, panel, directory := newRenderingBackend(t)
	backend.Fonts = latinLibrary(t)
	if _, err := backend.DisplayMarkdown(context.Background(), []byte("# Title\n\nBody text."), screen, DefaultMarkdownStyle()); err != nil {
		t.Fatalf("markdown rendering failed: %v", err)
	}
	if panel.calls() != 1 {
		t.Fatalf("the panel was called %d times", panel.calls())
	}
	if err := validateFullScreenPNG(readFile(t, filepath.Join(directory, currentImageName)), screen); err != nil {
		t.Fatalf("the committed markdown frame is not verifiable: %v", err)
	}
}

func TestRestoreLastScreenPrefersCurrentThenPreviousThenNothing(t *testing.T) {
	screen := ScreenCapabilities{Width: 12, Height: 8}
	directory := t.TempDir()
	store := NewDisplayStore(directory)
	panel := &recordingPanel{}
	first := screenPNG(t, screen, 0x10)
	second := screenPNG(t, screen, 0xa0)
	for _, payload := range [][]byte{first, second} {
		if _, err := store.Commit(context.Background(), payload, screen, panel.Show); err != nil {
			t.Fatal(err)
		}
	}

	recovered, err := RestoreLastScreen(context.Background(), store, panel, screen)
	if err != nil || recovered.Name != currentImageName || len(recovered.SHA256) != 64 {
		t.Fatalf("restore did not prefer current.png: %+v %v", recovered.Name, err)
	}
	if filepath.Base(panel.shown[len(panel.shown)-1]) != currentImageName {
		t.Fatal("restore did not hand the committed screen to the panel")
	}

	// A crash between the write and the durable rename leaves a truncated
	// current.png; previous.png has to take over rather than the panel being
	// handed unvalidated bytes.
	if err := os.WriteFile(filepath.Join(directory, currentImageName), second[:len(second)/2], 0600); err != nil {
		t.Fatal(err)
	}
	fallback, err := RestoreLastScreen(context.Background(), store, panel, screen)
	if err != nil || fallback.Name != previousImageName {
		t.Fatalf("restore did not fall back to previous.png: %+v %v", fallback.Name, err)
	}

	if err := os.WriteFile(filepath.Join(directory, previousImageName), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	before := panel.calls()
	if _, err := RestoreLastScreen(context.Background(), store, panel, screen); !errors.Is(err, ErrPersistence) {
		t.Fatalf("restore returned unvalidated data: %v", err)
	}
	if panel.calls() != before {
		t.Fatal("the panel was touched although nothing verified")
	}
}

func TestRestoreAtStartupLeavesTheScreenAloneAndReportsDiagnostics(t *testing.T) {
	backend, panel, _ := newRenderingBackend(t)
	state := NewStateStore(true)
	restoreLastScreenAtStartup(context.Background(), backend, &FakeScreen{Capabilities: ScreenCapabilities{Width: 12, Height: 8}}, state, io.Discard)
	snapshot := state.Snapshot("test-version", false, BackendStatus{})
	if snapshot.Current != nil || snapshot.LastError == nil || snapshot.LastError.Code != "persistence_failed" {
		t.Fatalf("an empty state directory was not reported as diagnosable: %+v", snapshot)
	}
	if panel.calls() != 0 {
		t.Fatal("the panel was touched with nothing to restore")
	}

	state = NewStateStore(true)
	restoreLastScreenAtStartup(context.Background(), backend, &FakeScreen{Err: errors.New("no framebuffer")}, state, io.Discard)
	snapshot = state.Snapshot("test-version", false, BackendStatus{})
	if snapshot.LastError == nil || snapshot.LastError.Code != "display_failed" || panel.calls() != 0 {
		t.Fatalf("an unprobeable panel was not reported safely: %+v", snapshot)
	}
}

// A first boot has no current/previous to restore. Leaving the panel white
// looks like a dead service, so the built-in help page is displayed through
// the normal transaction and becomes the first committed screen. It requires
// the font library; without fonts the panel is still left alone.
func TestStartupWithoutSavedScreenDisplaysTheDefaultHelpPage(t *testing.T) {
	backend, panel, _ := newRenderingBackend(t)
	backend.Fonts = installedCJKLibrary(t)
	state := NewStateStore(true)
	restoreLastScreenAtStartup(context.Background(), backend, &FakeScreen{Capabilities: ScreenCapabilities{Width: 600, Height: 800}}, state, io.Discard)
	snapshot := state.Snapshot("test-version", false, BackendStatus{})
	if panel.calls() != 1 {
		t.Fatalf("the default help page was not displayed exactly once: %d calls", panel.calls())
	}
	if snapshot.Current == nil || snapshot.LastError != nil {
		t.Fatalf("the default page was not committed as the current screen: %+v", snapshot)
	}
}

func TestStartupDefaultHelpPageNeverOverridesARestorableScreen(t *testing.T) {
	backend, panel, _ := newRenderingBackend(t)
	backend.Fonts = latinLibrary(t)
	screen := ScreenCapabilities{Width: 600, Height: 800}
	body := grayFixture(t, 20, 10, func(int, int) uint8 { return 0x20 })
	shown, err := backend.DisplayImage(context.Background(), "image/png", "contain", body, screen)
	if err != nil {
		t.Fatal(err)
	}
	before := panel.calls()
	state := NewStateStore(true)
	restoreLastScreenAtStartup(context.Background(), backend, &FakeScreen{Capabilities: screen}, state, io.Discard)
	if panel.calls() != before+1 {
		t.Fatalf("expected exactly the restore, not the default page: %d -> %d calls", before, panel.calls())
	}
	snapshot := state.Snapshot("test-version", false, BackendStatus{})
	if snapshot.Current == nil || snapshot.Current.SHA256 != shown.SHA256 {
		t.Fatalf("the restore did not bring back the saved screen: %+v", snapshot.Current)
	}
}

func TestDisplayEndpointMapsRenderSentinelsOntoTheFrozenContract(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		status   int
		code     string
		recorded bool
	}{
		{name: "dimensions", err: ErrImageDimensions, status: http.StatusRequestEntityTooLarge, code: "image_dimensions_exceeded"},
		{name: "decode", err: ErrDecodeFailed, status: http.StatusUnprocessableEntity, code: "decode_failed"},
		{name: "missing glyph", err: ErrMissingGlyph, status: http.StatusUnprocessableEntity, code: "render_failed"},
		{name: "markdown render", err: ErrMarkdownRender, status: http.StatusUnprocessableEntity, code: "render_failed"},
		{name: "persistence", err: ErrPersistence, status: http.StatusInternalServerError, code: "persistence_failed", recorded: true},
		{name: "fonts", err: ErrFontDigest, status: http.StatusInternalServerError, code: "display_failed", recorded: true},
		{name: "backend", err: ErrDisplayBackend, status: http.StatusInternalServerError, code: "display_failed", recorded: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, token, handler, display, _, _, state := newHarness(t)
			display.Err = test.err
			response := request(handler, http.MethodPut, "/v1/display/image", "image/png", strings.NewReader("payload"), token)
			if response.Code != test.status || errorCode(t, response) != test.code {
				t.Fatalf("got %d %s", response.Code, response.Body.String())
			}
			snapshot := state.Snapshot("test-version", false, BackendStatus{})
			if snapshot.Current != nil {
				t.Fatal("a failed transaction was committed")
			}
			if test.recorded {
				if snapshot.LastError == nil || snapshot.LastError.Code != test.code {
					t.Fatalf("a service-side failure was not recorded: %+v", snapshot.LastError)
				}
			} else if snapshot.LastError != nil {
				t.Fatalf("a client-content failure was recorded as service state: %+v", snapshot.LastError)
			}
		})
	}
}

func TestFBInkPanelInvokesTheExternalExecutable(t *testing.T) {
	directory := t.TempDir()
	log := filepath.Join(directory, "args.log")
	// The argument list is passed through the environment rather than embedded
	// in the script, so no path has to survive shell quoting.
	t.Setenv("EINKRELAY_TEST_FBINK_LOG", log)
	script := filepath.Join(directory, "fbink")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$EINKRELAY_TEST_FBINK_LOG\"\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(directory, "candidate.png")
	panel := &FBInkPanel{Path: script}
	if err := panel.Show(context.Background(), candidate); err != nil {
		t.Fatalf("the external backend call failed: %v", err)
	}
	recorded := string(readFile(t, log))
	if !strings.Contains(recorded, "file="+candidate) {
		t.Fatalf("the candidate path did not reach the backend: %q", recorded)
	}
	if status := panel.Status(context.Background()); status.Name != "fbink" || status.State != "ready" {
		t.Fatalf("unexpected backend status: %+v", status)
	}
}

func TestFBInkPanelFailsClosedAndReportsUnavailable(t *testing.T) {
	panel := &FBInkPanel{Path: filepath.Join(t.TempDir(), "absent-fbink")}
	err := panel.Show(context.Background(), "candidate.png")
	if !errors.Is(err, ErrDisplayBackend) {
		t.Fatalf("a missing backend did not fail closed: %v", err)
	}
	// The exec error carries the executable path; only the stable sentinel may
	// travel towards a response.
	if strings.Contains(err.Error(), "absent-fbink") {
		t.Fatal("the backend error leaked a device path")
	}
	status := panel.Status(context.Background())
	if status.State != "unavailable" || status.Version != nil {
		t.Fatalf("unexpected backend status: %+v", status)
	}
}

func TestFBInkPanelProbeReadsVersionFromTheHelpBanner(t *testing.T) {
	// A real FBInk CLI has no `--version` flag; the only probe it honours is
	// `--help`, whose first line carries the version banner.
	stub := writeFBInkStub(t, fbinkStubRealCLI)
	panel := &FBInkPanel{Path: stub}
	panel.Probe(context.Background())
	status := panel.Status(context.Background())
	if status.Version == nil || *status.Version != "FBInk v1.25.0 for Kindle [Draw=Yes]" {
		t.Fatalf("the help banner did not become the recorded version: %+v", status)
	}
}

func TestFBInkPanelProbeToleratesABackendWithoutHelp(t *testing.T) {
	stub := writeFBInkStub(t, "#!/bin/sh\nexit 1\n")
	panel := &FBInkPanel{Path: stub}
	panel.Probe(context.Background())
	if status := panel.Status(context.Background()); status.Version != nil {
		t.Fatalf("a failing probe must not record a version: %+v", status)
	}
}

func TestSanitizeBackendVersionKeepsOneShortPrintableLine(t *testing.T) {
	if got := sanitizeBackendVersion("  FBInk v1.25.0 \nextra line\n"); got != "FBInk v1.25.0" {
		t.Fatalf("unexpected version: %q", got)
	}
	if got := sanitizeBackendVersion(strings.Repeat("v", 200)); len(got) != maxBackendVersionLength {
		t.Fatalf("a chatty backend was not truncated: %d", len(got))
	}
	if got := sanitizeBackendVersion("\x00\x01\x02"); got != "" {
		t.Fatalf("control characters survived: %q", got)
	}
}
