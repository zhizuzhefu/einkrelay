package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func randomToken(t *testing.T) []byte {
	t.Helper()
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	return []byte(base64.RawURLEncoding.EncodeToString(value))
}

func newHarness(t *testing.T) (Config, []byte, *Handler, *FakeDisplay, *FakeScreen, *FakeNative, *StateStore) {
	t.Helper()
	cfg := DefaultConfig()
	token := randomToken(t)
	display := &FakeDisplay{Backend: BackendStatus{Name: "fake", State: "ready"}}
	screen := &FakeScreen{Capabilities: ScreenCapabilities{Width: 600, Height: 800}}
	native := &FakeNative{}
	state := NewStateStore(true)
	handler := NewHandler(cfg, "test-version", NewAuthenticator(token), display, native, screen, state)
	return cfg, token, handler, display, screen, native, state
}

func request(handler http.Handler, method, target, contentType string, body io.Reader, token []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != nil {
		req.Header.Set("Authorization", "Bearer "+string(token))
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func errorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var decoded APIError
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if decoded.Error.Message == "" {
		t.Fatal("error message is empty")
	}
	return decoded.Error.Code
}

func TestConfigDefaultsAndOverrides(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Address() != "0.0.0.0:8080" || cfg.ImageMaxBytes != 10*1024*1024 || cfg.MarkdownMaxBytes != 1024*1024 || cfg.ReadTimeout != 15*time.Second || cfg.TransactionTimeout != 60*time.Second || cfg.ImageMaxDimension != 8192 || cfg.ImageMaxPixels != 32000000 || cfg.GestureTapWindow != time.Second {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	environment := map[string]string{
		"EINKRELAY_LISTEN_ADDRESS":      "127.0.0.1",
		"EINKRELAY_LISTEN_PORT":         "18080",
		"EINKRELAY_IMAGE_MAX_BYTES":     "4096",
		"EINKRELAY_MARKDOWN_MAX_BYTES":  "2048",
		"EINKRELAY_READ_TIMEOUT":        "2s",
		"EINKRELAY_TRANSACTION_TIMEOUT": "3s",
		"EINKRELAY_GESTURE_TAP_WINDOW":  "1500ms",
	}
	loaded, err := LoadConfigFromEnv(func(name string) string { return environment[name] })
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Address() != "127.0.0.1:18080" || loaded.ImageMaxBytes != 4096 || loaded.MarkdownMaxBytes != 2048 || loaded.ReadTimeout != 2*time.Second || loaded.TransactionTimeout != 3*time.Second || loaded.GestureTapWindow != 1500*time.Millisecond {
		t.Fatalf("overrides not loaded: %+v", loaded)
	}
	environment["EINKRELAY_LISTEN_PORT"] = "0"
	if _, err := LoadConfigFromEnv(func(name string) string { return environment[name] }); err == nil {
		t.Fatal("invalid port accepted")
	}
}

func TestLoadTokenRequiresRegular0600File(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "token")
	token := randomToken(t)
	if err := os.WriteFile(path, token, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadToken(path)
	if err != nil || !bytes.Equal(loaded, token) {
		t.Fatalf("valid token rejected: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToken(path); err == nil {
		t.Fatal("insecure permissions accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToken(link); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := LoadToken(directory); err == nil {
		t.Fatal("directory accepted")
	}
}

func TestHealthzIsGoneAndAuthenticationKeepsPriority(t *testing.T) {
	_, token, handler, display, screen, native, state := newHarness(t)
	// The anonymous health endpoint was removed from the contract: /healthz is
	// now an unknown path and answers 404 like any other. /v1/status remains
	// the only liveness read, behind the bearer token.
	health := request(handler, http.MethodGet, "/healthz", "", nil, nil)
	if health.Code != http.StatusNotFound || errorCode(t, health) != "not_found" {
		t.Fatalf("the removed health endpoint did not answer 404: %d %s", health.Code, health.Body.String())
	}
	authenticated := request(handler, http.MethodGet, "/v1/status", "", nil, token)
	if authenticated.Code != http.StatusOK {
		t.Fatalf("status with token: %d %s", authenticated.Code, authenticated.Body.String())
	}
	// Counts are compared against a baseline rather than against zero: the
	// authenticated status read above legitimately probes the panel geometry,
	// and what this test is about is that the *unauthorized* request below
	// touches nothing.
	displayBefore, screenBefore, nativeBefore := display.Calls, screen.Calls, native.Calls
	unauthorized := request(handler, http.MethodPut, "/v1/display/image?fit=bad", "text/plain", strings.NewReader(strings.Repeat("x", 128)), nil)
	if unauthorized.Code != http.StatusUnauthorized || errorCode(t, unauthorized) != "unauthorized" {
		t.Fatalf("unexpected unauthorized response: %d", unauthorized.Code)
	}
	if display.Calls != displayBefore || screen.Calls != screenBefore || native.Calls != nativeBefore || state.Snapshot("test-version", false, BackendStatus{}).Current != nil {
		t.Fatal("unauthorized request caused a side effect")
	}
	unknown := request(handler, http.MethodGet, "/v1/unknown", "", nil, nil)
	if unknown.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated unknown route returned %d", unknown.Code)
	}
	authorizedUnknown := request(handler, http.MethodGet, "/v1/unknown", "", nil, token)
	if authorizedUnknown.Code != http.StatusNotFound || errorCode(t, authorizedUnknown) != "not_found" {
		t.Fatalf("authorized unknown route returned %d", authorizedUnknown.Code)
	}
	wrongMethod := request(handler, http.MethodPost, "/v1/status", "", nil, token)
	if wrongMethod.Code != http.StatusMethodNotAllowed || errorCode(t, wrongMethod) != "method_not_allowed" {
		t.Fatalf("wrong method returned %d", wrongMethod.Code)
	}
}

func TestDuplicateAndMalformedAuthorizationAreRejected(t *testing.T) {
	_, token, handler, display, _, _, _ := newHarness(t)
	cases := []func(*http.Request){
		func(r *http.Request) { r.Header.Set("Authorization", "Basic "+string(token)) },
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer") },
		func(r *http.Request) {
			r.Header.Add("Authorization", "Bearer "+string(token))
			r.Header.Add("Authorization", "Bearer "+string(token))
		},
	}
	for index, mutate := range cases {
		req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
		mutate(req)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("case %d returned %d", index, response.Code)
		}
	}
	if display.StatusCalls != 0 {
		t.Fatal("unauthorized status request reached backend")
	}
}

func TestEndpointContractAndStatusAggregation(t *testing.T) {
	_, token, handler, display, screen, native, _ := newHarness(t)
	status := request(handler, http.MethodGet, "/v1/status", "", nil, token)
	if status.Code != http.StatusOK {
		t.Fatalf("status returned %d", status.Code)
	}
	var initial StatusResponse
	if err := json.Unmarshal(status.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	if !initial.Active || initial.Mode != "active" || initial.Busy || initial.Current != nil || initial.Backend.Name != "fake" {
		t.Fatalf("unexpected initial status: %+v", initial)
	}
	// The panel geometry is what a caller sizes content against. Without it the
	// only way to fit an image is to know the device model by heart, which is
	// how a request ends up succeeding while the screen shows nothing useful.
	if initial.Screen == nil || initial.Screen.Width != 600 || initial.Screen.Height != 800 {
		t.Fatalf("status did not report the probed panel geometry: %+v", initial.Screen)
	}
	probesBeforeDisplay := screen.Calls
	image := request(handler, http.MethodPut, "/v1/display/image", "image/png", bytes.NewReader([]byte{0x89, 'P', 'N', 'G'}), token)
	if image.Code != http.StatusOK {
		t.Fatalf("image returned %d: %s", image.Code, image.Body.String())
	}
	var displayed StatusResponse
	if err := json.Unmarshal(image.Body.Bytes(), &displayed); err != nil {
		t.Fatal(err)
	}
	if displayed.Current == nil || len(displayed.Current.SHA256) != 64 || display.LastFit != "contain" || display.Calls != 1 {
		t.Fatalf("display state not aggregated: %+v", displayed)
	}
	if screen.Calls <= probesBeforeDisplay {
		t.Fatal("the display transaction did not probe the panel geometry")
	}
	markdown := request(handler, http.MethodPut, "/v1/display/markdown", "text/markdown; charset=utf-8", strings.NewReader("# 中文\n\nHello"), token)
	if markdown.Code != http.StatusOK || display.Calls != 2 {
		t.Fatalf("markdown returned %d", markdown.Code)
	}
	exit := request(handler, http.MethodPost, "/v1/system/exit", "", nil, token)
	if exit.Code != http.StatusOK || native.Calls != 1 {
		t.Fatalf("exit returned %d", exit.Code)
	}
	var exited StatusResponse
	if err := json.Unmarshal(exit.Body.Bytes(), &exited); err != nil {
		t.Fatal(err)
	}
	if exited.Active || exited.Mode != "inactive" {
		t.Fatalf("exit state not reflected: %+v", exited)
	}
	repeated := request(handler, http.MethodPost, "/v1/system/exit", "", nil, token)
	if repeated.Code != http.StatusOK || native.Calls != 2 {
		t.Fatalf("repeated exit was not idempotent: %d", repeated.Code)
	}
}

func TestRequestGuards(t *testing.T) {
	cfg, token, _, display, screen, native, state := newHarness(t)
	cfg.ImageMaxBytes = 4
	cfg.MarkdownMaxBytes = 4
	handler := NewHandler(cfg, "test-version", NewAuthenticator(token), display, native, screen, state)
	cases := []struct {
		name        string
		method      string
		target      string
		contentType string
		body        []byte
		encoding    string
		wantStatus  int
		wantCode    string
	}{
		{name: "invalid fit", method: http.MethodPut, target: "/v1/display/image?fit=stretch", contentType: "image/png", body: []byte("1234"), wantStatus: 400, wantCode: "invalid_parameter"},
		{name: "repeated fit", method: http.MethodPut, target: "/v1/display/image?fit=contain&fit=cover", contentType: "image/png", body: []byte("1234"), wantStatus: 400, wantCode: "invalid_parameter"},
		{name: "image media", method: http.MethodPut, target: "/v1/display/image", contentType: "text/plain", body: []byte("1234"), wantStatus: 415, wantCode: "unsupported_media_type"},
		{name: "image limit plus one", method: http.MethodPut, target: "/v1/display/image", contentType: "image/png", body: []byte("12345"), wantStatus: 413, wantCode: "payload_too_large"},
		{name: "markdown invalid utf8", method: http.MethodPut, target: "/v1/display/markdown", contentType: "text/markdown", body: []byte{0xff}, wantStatus: 400, wantCode: "invalid_encoding"},
		{name: "markdown charset", method: http.MethodPut, target: "/v1/display/markdown", contentType: "text/markdown; charset=iso-8859-1", body: []byte("1234"), wantStatus: 415, wantCode: "unsupported_media_type"},
		{name: "compressed", method: http.MethodPut, target: "/v1/display/image", contentType: "image/png", body: []byte("1234"), encoding: "gzip", wantStatus: 415, wantCode: "unsupported_content_encoding"},
		{name: "exit body", method: http.MethodPost, target: "/v1/system/exit", body: []byte("x"), wantStatus: 400, wantCode: "invalid_request"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.target, bytes.NewReader(test.body))
			req.Header.Set("Authorization", "Bearer "+string(token))
			if test.contentType != "" {
				req.Header.Set("Content-Type", test.contentType)
			}
			if test.encoding != "" {
				req.Header.Set("Content-Encoding", test.encoding)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != test.wantStatus || errorCode(t, response) != test.wantCode {
				t.Fatalf("got %d %s", response.Code, response.Body.String())
			}
		})
	}
	if display.Calls != 0 || screen.Calls != 0 || state.Snapshot("test-version", false, BackendStatus{}).Current != nil {
		t.Fatal("guard rejection reached display transaction")
	}
	exact := request(handler, http.MethodPut, "/v1/display/image?fit=cover", "image/jpeg", strings.NewReader("1234"), token)
	if exact.Code != http.StatusOK || display.Calls != 1 || display.LastFit != "cover" {
		t.Fatalf("exact limit request failed: %d", exact.Code)
	}
}

// TestPayloadTooLargeHasNoFilesystemEffect proves the 413 guard per case and
// against a real state directory. The aggregate assertion in TestRequestGuards
// only inspects in-memory fakes, so it cannot tell a rejection that wrote
// nothing apart from one that wrote a candidate and then cleaned it up, nor
// one that disturbed the screen already on the panel.
func TestPayloadTooLargeHasNoFilesystemEffect(t *testing.T) {
	screen := ScreenCapabilities{Width: 40, Height: 30}
	oversized := []byte(strings.Repeat("x", 64))
	cases := []struct {
		name          string
		target        string
		contentType   string
		unknownLength bool
	}{
		{name: "image with a declared length", target: "/v1/display/image", contentType: "image/png"},
		{name: "image with an unknown length", target: "/v1/display/image", contentType: "image/png", unknownLength: true},
		{name: "markdown with a declared length", target: "/v1/display/markdown", contentType: "text/markdown; charset=utf-8"},
		{name: "markdown with an unknown length", target: "/v1/display/markdown", contentType: "text/markdown; charset=utf-8", unknownLength: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			panel := &recordingPanel{}
			backend := &RenderingBackend{
				Store:  NewDisplayStore(directory),
				Panel:  panel,
				Limits: DefaultImageLimits(),
				Style:  DefaultMarkdownStyle(),
			}
			cfg := DefaultConfig()
			cfg.ImageMaxBytes = 8
			cfg.MarkdownMaxBytes = 8
			token := randomToken(t)
			state := NewStateStore(true)
			handler := NewHandler(cfg, "test-version", NewAuthenticator(token), backend, &FakeNative{}, &FakeScreen{Capabilities: screen}, state)

			reject := func() {
				t.Helper()
				req := httptest.NewRequest(http.MethodPut, test.target, bytes.NewReader(oversized))
				req.Header.Set("Content-Type", test.contentType)
				req.Header.Set("Authorization", "Bearer "+string(token))
				if test.unknownLength {
					// A declared length is refused before the body is touched;
					// clearing it forces the rejection through the read limit
					// instead, which is the branch that owns a partially
					// consumed body.
					req.ContentLength = -1
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, req)
				if response.Code != http.StatusRequestEntityTooLarge || errorCode(t, response) != "payload_too_large" {
					t.Fatalf("got %d %s", response.Code, response.Body.String())
				}
			}

			reject()
			if panel.calls() != 0 {
				t.Fatal("a rejected request reached the panel")
			}
			assertNoCandidates(t, directory)
			if _, err := os.Stat(filepath.Join(directory, currentImageName)); !os.IsNotExist(err) {
				t.Fatalf("a rejected request produced current.png: %v", err)
			}

			// Repeat against a store that already holds a displayed screen, so
			// the assertion distinguishes "never created" from "created and then
			// destroyed".
			seed := grayFixture(t, 20, 10, func(int, int) uint8 { return 0x20 })
			if _, err := backend.DisplayImage(context.Background(), "image/png", "contain", seed, screen); err != nil {
				t.Fatalf("seeding a committed screen failed: %v", err)
			}
			committed := readFile(t, filepath.Join(directory, currentImageName))
			shown := panel.calls()

			reject()
			if panel.calls() != shown {
				t.Fatal("a rejected request reached the panel")
			}
			assertNoCandidates(t, directory)
			if !bytes.Equal(readFile(t, filepath.Join(directory, currentImageName)), committed) {
				t.Fatal("a rejected request rewrote the committed screen")
			}
			if _, err := os.Stat(filepath.Join(directory, previousImageName)); !os.IsNotExist(err) {
				t.Fatalf("a rejected request rotated the committed screen: %v", err)
			}
			if snapshot := state.Snapshot("test-version", false, BackendStatus{}); snapshot.Current != nil || snapshot.LastError != nil {
				t.Fatalf("a rejected request changed service state: %+v", snapshot)
			}
		})
	}
}

func TestDisplayBusyIsImmediateAndShared(t *testing.T) {
	cfg, token, _, display, screen, native, state := newHarness(t)
	display.Entered = make(chan struct{})
	display.Release = make(chan struct{})
	handler := NewHandler(cfg, "test-version", NewAuthenticator(token), display, native, screen, state)
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- request(handler, http.MethodPut, "/v1/display/image", "image/png", strings.NewReader("image"), token)
	}()
	<-display.Entered
	busy := request(handler, http.MethodPut, "/v1/display/markdown", "text/plain", strings.NewReader("bad-media"), token)
	if busy.Code != http.StatusConflict || errorCode(t, busy) != "display_busy" {
		t.Fatalf("busy request returned %d", busy.Code)
	}
	if display.Calls != 1 {
		t.Fatalf("busy request reached backend; calls=%d", display.Calls)
	}
	close(display.Release)
	if first := <-firstDone; first.Code != http.StatusOK {
		t.Fatalf("first request returned %d", first.Code)
	}
	third := request(handler, http.MethodPut, "/v1/display/markdown", "text/markdown", strings.NewReader("ready"), token)
	if third.Code != http.StatusOK || display.Calls != 2 || display.MaxActive != 1 {
		t.Fatalf("lock did not recover: status=%d calls=%d max=%d", third.Code, display.Calls, display.MaxActive)
	}
}

func TestTransactionTimeoutCancelsBackendBeforeUnlock(t *testing.T) {
	cfg, token, _, display, screen, native, state := newHarness(t)
	cfg.TransactionTimeout = 10 * time.Millisecond
	display.Entered = make(chan struct{})
	display.BlockUntilCancel = true
	handler := NewHandler(cfg, "test-version", NewAuthenticator(token), display, native, screen, state)
	response := request(handler, http.MethodPut, "/v1/display/image", "image/png", strings.NewReader("image"), token)
	if response.Code != http.StatusGatewayTimeout || errorCode(t, response) != "transaction_timeout" {
		t.Fatalf("timeout returned %d %s", response.Code, response.Body.String())
	}
	display.mu.Lock()
	active, maximum := display.Active, display.MaxActive
	display.mu.Unlock()
	if active != 0 || maximum != 1 || handler.busy.Load() {
		t.Fatalf("backend remained active: active=%d max=%d busy=%v", active, maximum, handler.busy.Load())
	}
	status := state.Snapshot("test-version", false, BackendStatus{})
	if status.Current != nil || status.LastError == nil || status.LastError.Code != "transaction_timeout" {
		t.Fatalf("timeout state was unsafe: %+v", status)
	}
}

type timeoutReadCloser struct{}

func (timeoutReadCloser) Read([]byte) (int, error) { return 0, timeoutError{} }
func (timeoutReadCloser) Close() error             { return nil }

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestBodyReadTimeoutReturns408JSON(t *testing.T) {
	_, token, handler, display, _, _, _ := newHarness(t)
	req := httptest.NewRequest(http.MethodPut, "/v1/display/markdown", nil)
	req.Body = timeoutReadCloser{}
	req.ContentLength = -1
	req.Header.Set("Content-Type", "text/markdown")
	req.Header.Set("Authorization", "Bearer "+string(token))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusRequestTimeout || errorCode(t, response) != "request_timeout" || display.Calls != 0 {
		t.Fatalf("read timeout returned %d", response.Code)
	}
}

func TestBackendErrorMappingsDoNotCommit(t *testing.T) {
	cfg, token, _, display, screen, native, state := newHarness(t)
	handler := NewHandler(cfg, "test-version", NewAuthenticator(token), display, native, screen, state)
	display.Err = ErrUnprocessable
	unprocessable := request(handler, http.MethodPut, "/v1/display/image", "image/png", strings.NewReader("image"), token)
	if unprocessable.Code != http.StatusUnprocessableEntity || errorCode(t, unprocessable) != "render_failed" {
		t.Fatalf("unprocessable returned %d", unprocessable.Code)
	}
	if state.Snapshot("test-version", false, BackendStatus{}).Current != nil {
		t.Fatal("unprocessable content was committed")
	}
	display.Err = errors.New("sensitive underlying failure")
	failed := request(handler, http.MethodPut, "/v1/display/image", "image/png", strings.NewReader("image"), token)
	if failed.Code != http.StatusInternalServerError || errorCode(t, failed) != "display_failed" || strings.Contains(failed.Body.String(), "sensitive") {
		t.Fatalf("backend failure leaked: %s", failed.Body.String())
	}
}

func TestHTTPServerTimeoutConfiguration(t *testing.T) {
	cfg := DefaultConfig()
	server := NewHTTPServer(cfg, http.NotFoundHandler())
	if server.Addr != "0.0.0.0:8080" || server.ReadHeaderTimeout != 15*time.Second || server.ReadTimeout != 15*time.Second || server.MaxHeaderBytes != 16*1024 {
		t.Fatalf("unexpected server configuration: %+v", server)
	}
}

func TestVersionAndUsageSubcommands(t *testing.T) {
	oldVersion := version
	version = "v0.1-test"
	t.Cleanup(func() { version = oldVersion })
	var stdout, stderr bytes.Buffer
	if code := runCLI(context.Background(), []string{"version"}, &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != "v0.1-test" {
		t.Fatalf("version failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runCLI(context.Background(), []string{"unknown"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "serve|guard|version|preflight") {
		t.Fatalf("usage failed: code=%d stderr=%q", code, stderr.String())
	}
}

// writeFBInkStub installs a fake backend that behaves like a real FBInk CLI:
// it only honours `--help` (exit 0); anything else exits non-zero, exactly the
// way upstream FBInk rejects the `--version` flag it has never had.
func writeFBInkStub(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fbink")
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

const fbinkStubRealCLI = "#!/bin/sh\nif [ \"$1\" = \"--help\" ]; then printf '\\nFBInk v1.25.0 for Kindle [Draw=Yes]\\n\\nUsage...\\n'; exit 0; fi\necho 'unrecognized option' >&2\nexit 255\n"

func TestCheckPreflightAcceptsARealFBInkThatOnlyKnowsHelp(t *testing.T) {
	path := writeFBInkStub(t, fbinkStubRealCLI)
	sum := sha256.Sum256([]byte(fbinkStubRealCLI))
	if err := CheckPreflight(context.Background(), path, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("preflight rejected a backend that answers --help: %v", err)
	}
	// No digest pinning must stay supported for the operator-provided path.
	if err := CheckPreflight(context.Background(), path, ""); err != nil {
		t.Fatalf("preflight without a digest rejected the backend: %v", err)
	}
}

func TestCheckPreflightStillFailsClosed(t *testing.T) {
	path := writeFBInkStub(t, fbinkStubRealCLI)
	if err := CheckPreflight(context.Background(), path, digestFixture("a")); err == nil {
		t.Fatal("a digest mismatch was not rejected")
	}
	if err := CheckPreflight(context.Background(), filepath.Join(t.TempDir(), "absent"), ""); err == nil {
		t.Fatal("a missing backend was not rejected")
	}
	dead := writeFBInkStub(t, "#!/bin/sh\nexit 1\n")
	if err := CheckPreflight(context.Background(), dead, ""); err == nil {
		t.Fatal("a backend that cannot answer --help was not rejected")
	}
	// A file without the executable bit must fail even when the digest matches.
	noExec := filepath.Join(t.TempDir(), "fbink")
	if err := os.WriteFile(noExec, []byte(fbinkStubRealCLI), 0644); err != nil {
		t.Fatal(err)
	}
	if err := CheckPreflight(context.Background(), noExec, ""); err == nil {
		t.Fatal("a non-executable backend was not rejected")
	}
}

// digestFixture is a syntactically valid sha256 hex string. The state store only
// carries the value through, so the tests that exercise it do not need a real
// image behind it.
func digestFixture(nibble string) string { return strings.Repeat(nibble, 64) }

// hexRunFinder matches the sort of long hex run a digest would leave behind if
// one ever reached a status message.
var hexRunFinder = regexp.MustCompile(`[0-9a-fA-F]{16,}`)

// assertStatusMessageIsSafe holds every sanitized status message to the same
// rule: it may describe the condition but may never name the thing. A path, a
// download URL, a pinned digest or the bearer token would each turn a status
// poll into an information leak, and /v1/status is the one endpoint a monitoring
// system is expected to scrape continuously.
func assertStatusMessageIsSafe(t *testing.T, where, message string, token []byte) {
	t.Helper()
	if message == "" {
		t.Fatalf("%s: the status message is empty", where)
	}
	for _, forbidden := range []string{"/", "\\", "://", "http", "sha256", ".ttf", ".otf", "Bearer"} {
		if strings.Contains(strings.ToLower(message), strings.ToLower(forbidden)) {
			t.Fatalf("%s: the status message leaked %q: %q", where, forbidden, message)
		}
	}
	if token != nil && strings.Contains(message, string(token)) {
		t.Fatalf("%s: the status message leaked the bearer token: %q", where, message)
	}
	if found := hexRunFinder.FindString(message); found != "" {
		t.Fatalf("%s: the status message leaked a digest-like run %q: %q", where, found, message)
	}
}

// TestPersistentConditionSurvivesASuccessfulDisplay is the core of the defect: a
// font fault recorded at startup is a statement about the device, not about the
// last transaction. A screen going up disproves a transient display failure, but
// it disproves nothing about fonts that do not verify — an image renders
// perfectly well while Markdown stays broken. Clearing the condition on Commit
// therefore hides a fault that only a repair on the device can end.
func TestPersistentConditionSurvivesASuccessfulDisplay(t *testing.T) {
	state := NewStateStore(true)
	message := fontStatusMessage(ErrFontDigest)
	before := time.Now().UTC()
	state.SetPersistentError("display_failed", message)

	initial := state.Snapshot("test-version", false, BackendStatus{})
	if initial.LastError == nil {
		t.Fatal("the persistent condition was not visible before any display")
	}
	if initial.LastError.Code != "display_failed" || initial.LastError.Message != message {
		t.Fatalf("unexpected persistent condition: %+v", initial.LastError)
	}
	if initial.LastError.At.Before(before) || initial.LastError.At.Location() != time.UTC {
		t.Fatalf("the persistent condition was not timestamped in UTC: %+v", initial.LastError)
	}

	displayed := time.Now().UTC()
	digest := digestFixture("a")
	state.Commit(DisplayResult{SHA256: digest, DisplayedAt: displayed})

	after := state.Snapshot("test-version", false, BackendStatus{})
	if after.Current == nil || after.Current.SHA256 != digest || !after.Current.DisplayedAt.Equal(displayed) {
		t.Fatalf("the successful display was not recorded: %+v", after.Current)
	}
	if after.LastError == nil {
		t.Fatal("a successful display erased the font condition it had not disproved")
	}
	if after.LastError.Code != "display_failed" || after.LastError.Message != message {
		t.Fatalf("the restored condition changed: %+v", after.LastError)
	}
	assertStatusMessageIsSafe(t, "restored persistent condition", after.LastError.Message, nil)
}

// TestCommitStillClearsATransientFailure guards the behaviour that has to be
// kept: without a persistent condition, a successful display is exactly the
// proof that the previous transient failure is over, so it must still clear.
func TestCommitStillClearsATransientFailure(t *testing.T) {
	state := NewStateStore(true)
	state.RecordError("persistence_failed", "no verifiable screen was available to restore")
	if snapshot := state.Snapshot("test-version", false, BackendStatus{}); snapshot.LastError == nil {
		t.Fatal("a transient failure was not recorded")
	}

	digest := digestFixture("b")
	state.Commit(DisplayResult{SHA256: digest, DisplayedAt: time.Now().UTC()})

	snapshot := state.Snapshot("test-version", false, BackendStatus{})
	if snapshot.Current == nil || snapshot.Current.SHA256 != digest {
		t.Fatalf("the successful display was not recorded: %+v", snapshot.Current)
	}
	if snapshot.LastError != nil {
		t.Fatalf("a successful display did not clear the transient failure it disproved: %+v", snapshot.LastError)
	}
}

// TestTransientFailureShadowsThePersistentConditionUntilTheNextCommit describes
// the ordering: the fresher fact is the more useful one to show while it is
// true, and when a successful display disproves it the reported state falls back
// to the condition that is still true rather than to nothing at all.
func TestTransientFailureShadowsThePersistentConditionUntilTheNextCommit(t *testing.T) {
	state := NewStateStore(true)
	fontMessage := fontStatusMessage(ErrFontMissing)
	state.SetPersistentError("display_failed", fontMessage)
	state.RecordError("transaction_timeout", "display transaction timed out")

	shadowed := state.Snapshot("test-version", false, BackendStatus{})
	if shadowed.LastError == nil || shadowed.LastError.Code != "transaction_timeout" {
		t.Fatalf("the newer transient failure was not the reported one: %+v", shadowed.LastError)
	}

	state.Commit(DisplayResult{SHA256: digestFixture("c"), DisplayedAt: time.Now().UTC()})

	restored := state.Snapshot("test-version", false, BackendStatus{})
	if restored.LastError == nil {
		t.Fatal("clearing the transient failure also discarded the persistent condition")
	}
	if restored.LastError.Code != "display_failed" || restored.LastError.Message != fontMessage {
		t.Fatalf("the persistent condition was not restored: %+v", restored.LastError)
	}
}

// TestClearingARepairedPersistentCondition makes the lifetime explicit: font
// failures survive unrelated display successes, but they are not immortal. A
// caller that has re-verified and reloaded the fonts may clear the condition.
// Clearing it must not erase a newer transaction failure that happens to be
// shadowing it at the time.
func TestClearingARepairedPersistentCondition(t *testing.T) {
	state := NewStateStore(true)
	state.SetPersistentError("display_failed", fontStatusMessage(ErrFontDigest))
	state.ClearPersistentError()

	if snapshot := state.Snapshot("test-version", false, BackendStatus{}); snapshot.LastError != nil {
		t.Fatalf("the repaired font condition remained visible: %+v", snapshot.LastError)
	}

	state.SetPersistentError("display_failed", fontStatusMessage(ErrFontMissing))
	state.RecordError("transaction_timeout", "display transaction timed out")
	state.ClearPersistentError()

	shadowed := state.Snapshot("test-version", false, BackendStatus{})
	if shadowed.LastError == nil || shadowed.LastError.Code != "transaction_timeout" {
		t.Fatalf("clearing the font condition erased a newer transient failure: %+v", shadowed.LastError)
	}

	state.Commit(DisplayResult{SHA256: digestFixture("c"), DisplayedAt: time.Now().UTC()})
	if afterCommit := state.Snapshot("test-version", false, BackendStatus{}); afterCommit.LastError != nil {
		t.Fatalf("the repaired condition returned after a successful display: %+v", afterCommit.LastError)
	}
}

// TestSnapshotHandsOutCopiesOfTheReportedState proves the reported state cannot
// be edited from outside. Restoring the persistent condition on every Commit
// makes this sharper than before: a stored pointer handed to a caller would let
// one status reader rewrite a device fault for every later reader.
func TestSnapshotHandsOutCopiesOfTheReportedState(t *testing.T) {
	state := NewStateStore(true)
	message := fontStatusMessage(ErrFontManifest)
	digest := digestFixture("d")
	displayed := time.Now().UTC()
	state.SetPersistentError("display_failed", message)
	state.Commit(DisplayResult{SHA256: digest, DisplayedAt: displayed})

	first := state.Snapshot("test-version", false, BackendStatus{})
	if first.LastError == nil || first.Current == nil {
		t.Fatalf("the snapshot is incomplete: %+v", first)
	}
	first.LastError.Code = "mutated"
	first.LastError.Message = "the fonts under the state directory did not verify"
	first.LastError.At = time.Unix(0, 0).UTC()
	first.Current.SHA256 = digestFixture("e")
	first.Current.DisplayedAt = time.Unix(0, 0).UTC()

	second := state.Snapshot("test-version", false, BackendStatus{})
	if second.LastError == first.LastError || second.Current == first.Current {
		t.Fatal("the snapshot handed out the stored pointers")
	}
	if second.LastError == nil || second.LastError.Code != "display_failed" || second.LastError.Message != message {
		t.Fatalf("a caller rewrote the stored condition through its snapshot: %+v", second.LastError)
	}
	if second.Current == nil || second.Current.SHA256 != digest || !second.Current.DisplayedAt.Equal(displayed) {
		t.Fatalf("a caller rewrote the stored screen through its snapshot: %+v", second.Current)
	}

	// The condition is restored on every Commit, so the copy has to be made
	// there too: a snapshot taken after a second display must not be able to
	// reach back into the persistent condition either.
	state.Commit(DisplayResult{SHA256: digestFixture("f"), DisplayedAt: time.Now().UTC()})
	third := state.Snapshot("test-version", false, BackendStatus{})
	if third.LastError == nil {
		t.Fatal("the persistent condition was lost on the second display")
	}
	third.LastError.Message = "rewritten by a status reader"
	state.Commit(DisplayResult{SHA256: digestFixture("a"), DisplayedAt: time.Now().UTC()})
	fourth := state.Snapshot("test-version", false, BackendStatus{})
	if fourth.LastError == nil || fourth.LastError.Message != message {
		t.Fatalf("the persistent condition was mutated through a snapshot: %+v", fourth.LastError)
	}
}

// TestStatusKeepsReportingTheFontConditionAfterASuccessfulDisplay runs the same
// defect through the HTTP surface, which is where it actually costs something:
// a device with unverifiable fonts serves images forever while a monitoring
// system polling /v1/status sees a clean service and Markdown stays broken.
func TestStatusKeepsReportingTheFontConditionAfterASuccessfulDisplay(t *testing.T) {
	_, token, handler, display, _, _, state := newHarness(t)
	message := fontStatusMessage(ErrFontDigest)
	state.SetPersistentError("display_failed", message)

	body := []byte{0x89, 'P', 'N', 'G'}
	sum := sha256.Sum256(body)
	expected := hex.EncodeToString(sum[:])

	displayResponse := request(handler, http.MethodPut, "/v1/display/image", "image/png", bytes.NewReader(body), token)
	if displayResponse.Code != http.StatusOK || display.Calls != 1 {
		t.Fatalf("the image request failed: %d %s", displayResponse.Code, displayResponse.Body.String())
	}

	statusResponse := request(handler, http.MethodGet, "/v1/status", "", nil, token)
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("status returned %d", statusResponse.Code)
	}
	var decoded StatusResponse
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Current == nil || decoded.Current.SHA256 != expected {
		t.Fatalf("the newly displayed screen was not reported: %+v", decoded.Current)
	}
	if decoded.LastError == nil {
		t.Fatal("a successful image display cleared the font condition from /v1/status")
	}
	if decoded.LastError.Code != "display_failed" || decoded.LastError.Message != message {
		t.Fatalf("unexpected condition in /v1/status: %+v", decoded.LastError)
	}
	assertStatusMessageIsSafe(t, "/v1/status last_error", decoded.LastError.Message, token)

	// The response the display request itself returns is the same document, so
	// the caller that just succeeded is told about the condition too.
	var afterDisplay StatusResponse
	if err := json.Unmarshal(displayResponse.Body.Bytes(), &afterDisplay); err != nil {
		t.Fatal(err)
	}
	if afterDisplay.LastError == nil || afterDisplay.LastError.Code != "display_failed" {
		t.Fatalf("the display response hid the font condition: %+v", afterDisplay.LastError)
	}

	// The frozen field names carry it, not just the decoded struct.
	var raw struct {
		LastError *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"last_error"`
	}
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw.LastError == nil || raw.LastError.Code != "display_failed" || raw.LastError.Message != message {
		t.Fatalf("last_error was not carried on the frozen field: %s", statusResponse.Body.String())
	}
}

// TestFontStatusMessagesNameTheConditionAndNothingElse pins the wording that
// reaches both stderr and /v1/status for every font failure mode, including one
// wrapping an error whose own text is full of the detail that must not travel.
func TestFontStatusMessagesNameTheConditionAndNothingElse(t *testing.T) {
	token := randomToken(t)
	leaky := errors.New("open /mnt/us/einkrelay/fonts/DejaVuSans.ttf: sha256 mismatch, expected 3b1f9c0d4e5a6b7c8d9e0f1a2b3c4d5e, fetched from https://example.invalid/fonts.tar.gz")
	for name, err := range map[string]error{
		"digest mismatch":   ErrFontDigest,
		"not installed":     ErrFontMissing,
		"unusable manifest": ErrFontManifest,
		"leaky underlying":  leaky,
	} {
		t.Run(name, func(t *testing.T) {
			assertStatusMessageIsSafe(t, name, fontStatusMessage(err), token)
		})
	}

	// The same wording has to survive the round trip into the reported state.
	state := NewStateStore(true)
	state.SetPersistentError("display_failed", fontStatusMessage(leaky))
	state.Commit(DisplayResult{SHA256: digestFixture("a"), DisplayedAt: time.Now().UTC()})
	snapshot := state.Snapshot("test-version", false, BackendStatus{})
	if snapshot.LastError == nil {
		t.Fatal("the persistent condition did not survive the commit")
	}
	assertStatusMessageIsSafe(t, "reported persistent condition", snapshot.LastError.Message, token)
}

// staticBackend reports exactly the BackendStatus it is given, including
// documents the frozen Status schema forbids. FakeDisplay cannot express this
// case: it substitutes a valid default whenever the configured name is empty.
// The display and markdown calls are delegated so the same fake can drive the
// 200 path of the display endpoints.
type staticBackend struct {
	DisplayBackend
	status BackendStatus
}

func (b *staticBackend) Status(context.Context) BackendStatus { return b.status }

func newStaticBackendHandler(t *testing.T, backend BackendStatus) (*Handler, []byte, *FakeDisplay, *StateStore) {
	t.Helper()
	inner := &FakeDisplay{Backend: BackendStatus{Name: "fake", State: "ready"}}
	token := randomToken(t)
	state := NewStateStore(true)
	handler := NewHandler(
		DefaultConfig(),
		"test-version",
		NewAuthenticator(token),
		&staticBackend{DisplayBackend: inner, status: backend},
		&FakeNative{},
		&FakeScreen{Capabilities: ScreenCapabilities{Width: 600, Height: 800}},
		state,
	)
	return handler, token, inner, state
}

// assertNoStatusDocumentLeaked proves the failure answered instead of the status
// snapshot, rather than in addition to it: a 500 whose body still carries the
// schema-invalid document would defeat the point of the check.
func assertNoStatusDocumentLeaked(t *testing.T, where string, response *httptest.ResponseRecorder) {
	t.Helper()
	body := response.Body.String()
	for _, field := range []string{`"mode"`, `"backend"`, `"busy"`, `"active"`, `"last_error"`} {
		if strings.Contains(body, field) {
			t.Fatalf("%s: a schema-invalid status document was emitted: %s", where, body)
		}
	}
}

// assertFrozenErrorEnvelope holds a failure response to the frozen
// ErrorResponse schema exactly: one `error` object carrying exactly `code` and
// `message` and nothing else, with the code inside the frozen ErrorCode enum.
// The adjudication on D1 requires the 500 to leak no token, no path and no
// device secret, so the sanitizing rule that already governs the /v1/status
// last_error message is applied to the envelope message as well.
func assertFrozenErrorEnvelope(t *testing.T, where string, response *httptest.ResponseRecorder, wantCode string, token []byte) {
	t.Helper()
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("%s: unexpected content type %q", where, got)
	}
	var envelope map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("%s: decode error response: %v", where, err)
	}
	if len(envelope) != 1 {
		t.Fatalf("%s: the error envelope carried extra fields: %s", where, response.Body.String())
	}
	inner, ok := envelope["error"].(map[string]any)
	if !ok {
		t.Fatalf("%s: the error envelope has no error object: %s", where, response.Body.String())
	}
	if len(inner) != 2 {
		t.Fatalf("%s: the error object carried extra fields: %s", where, response.Body.String())
	}
	code, _ := inner["code"].(string)
	message, _ := inner["message"].(string)
	if code != wantCode {
		t.Fatalf("%s: unexpected error code %q: %s", where, code, response.Body.String())
	}
	if !isFrozenErrorCode(code) {
		t.Fatalf("%s: the error code is outside the frozen enum: %q", where, code)
	}
	assertStatusMessageIsSafe(t, where, message, token)
}

// TestStatusFailsClosedOnASchemaInvalidSnapshot is deviation D1. The frozen
// contract declares 500 InternalError on getStatus, and the only honest way to
// produce it is to refuse to serve a 200 whose body does not satisfy the frozen
// Status schema. A display backend that reports an empty name or an out-of-enum
// state is the reachable source of such a document.
func TestStatusFailsClosedOnASchemaInvalidSnapshot(t *testing.T) {
	cases := []struct {
		name    string
		backend BackendStatus
	}{
		{name: "empty backend name", backend: BackendStatus{Name: "", State: "ready"}},
		{name: "out of enum backend state", backend: BackendStatus{Name: "fbink", State: "degraded"}},
		{name: "empty backend state", backend: BackendStatus{Name: "fbink", State: ""}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			handler, token, inner, _ := newStaticBackendHandler(t, test.backend)

			status := request(handler, http.MethodGet, "/v1/status", "", nil, token)
			if status.Code != http.StatusInternalServerError || errorCode(t, status) != "internal_error" {
				t.Fatalf("GET /v1/status returned %d %s", status.Code, status.Body.String())
			}
			assertNoStatusDocumentLeaked(t, "GET /v1/status", status)
			assertFrozenErrorEnvelope(t, "GET /v1/status", status, "internal_error", token)

			// writeStatus is also the 200 path of the display endpoints and of
			// /v1/system/exit. All three declare 500 InternalError, so failing
			// closed there introduces no undeclared response either.
			image := request(handler, http.MethodPut, "/v1/display/image", "image/png", strings.NewReader("image"), token)
			if image.Code != http.StatusInternalServerError || errorCode(t, image) != "internal_error" {
				t.Fatalf("PUT /v1/display/image returned %d %s", image.Code, image.Body.String())
			}
			assertNoStatusDocumentLeaked(t, "PUT /v1/display/image", image)
			assertFrozenErrorEnvelope(t, "PUT /v1/display/image", image, "internal_error", token)
			if inner.Calls != 1 {
				t.Fatalf("the display port was not reached exactly once: %d", inner.Calls)
			}

			exit := request(handler, http.MethodPost, "/v1/system/exit", "", nil, token)
			if exit.Code != http.StatusInternalServerError || errorCode(t, exit) != "internal_error" {
				t.Fatalf("POST /v1/system/exit returned %d %s", exit.Code, exit.Body.String())
			}
			assertNoStatusDocumentLeaked(t, "POST /v1/system/exit", exit)
			assertFrozenErrorEnvelope(t, "POST /v1/system/exit", exit, "internal_error", token)
		})
	}
}

// TestStatusValidationCoversTheFrozenSchemaConstraints walks the remaining
// Status constraints over the HTTP surface: a version that is not minLength 1, a
// current.sha256 that does not match ^[0-9a-f]{64}$, and a last_error.code
// outside the frozen ErrorCode enum. The control case proves the check does not
// simply refuse everything.
func TestStatusValidationCoversTheFrozenSchemaConstraints(t *testing.T) {
	cases := []struct {
		name    string
		version string
		mutate  func(*StateStore)
		want    int
	}{
		{name: "valid snapshot", version: "test-version", want: http.StatusOK},
		{
			name:    "valid snapshot with a committed screen and a recorded failure",
			version: "test-version",
			mutate: func(state *StateStore) {
				state.Commit(DisplayResult{SHA256: digestFixture("a"), DisplayedAt: time.Now().UTC()})
				state.RecordError("display_failed", "display transaction failed")
			},
			want: http.StatusOK,
		},
		{name: "empty version", version: "", want: http.StatusInternalServerError},
		{
			name:    "uppercase digest",
			version: "test-version",
			mutate: func(state *StateStore) {
				state.Commit(DisplayResult{SHA256: strings.ToUpper(digestFixture("a")), DisplayedAt: time.Now().UTC()})
			},
			want: http.StatusInternalServerError,
		},
		{
			name:    "short digest",
			version: "test-version",
			mutate: func(state *StateStore) {
				state.Commit(DisplayResult{SHA256: "abc123", DisplayedAt: time.Now().UTC()})
			},
			want: http.StatusInternalServerError,
		},
		{
			name:    "last_error code outside the frozen enum",
			version: "test-version",
			mutate: func(state *StateStore) {
				state.RecordError("mutated", "display transaction failed")
			},
			want: http.StatusInternalServerError,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			token := randomToken(t)
			state := NewStateStore(true)
			if test.mutate != nil {
				test.mutate(state)
			}
			handler := NewHandler(
				DefaultConfig(),
				test.version,
				NewAuthenticator(token),
				&FakeDisplay{Backend: BackendStatus{Name: "fake", State: "ready"}},
				&FakeNative{},
				&FakeScreen{Capabilities: ScreenCapabilities{Width: 600, Height: 800}},
				state,
			)
			response := request(handler, http.MethodGet, "/v1/status", "", nil, token)
			if response.Code != test.want {
				t.Fatalf("GET /v1/status returned %d %s", response.Code, response.Body.String())
			}
			if test.want == http.StatusInternalServerError {
				if errorCode(t, response) != "internal_error" {
					t.Fatalf("unexpected error code: %s", response.Body.String())
				}
				assertNoStatusDocumentLeaked(t, test.name, response)
				assertFrozenErrorEnvelope(t, test.name, response, "internal_error", token)
			}
		})
	}
}

// TestStatusSchemaValidatorRejectsAnInconsistentMode covers the one Status
// constraint the state store cannot violate through the HTTP surface, because
// Snapshot derives mode from active. The validator still has to enforce it: it
// is the check that would catch a future regression in that derivation before it
// reaches a client.
func TestStatusSchemaValidatorRejectsAnInconsistentMode(t *testing.T) {
	valid := StatusResponse{
		Version: "test-version",
		Active:  true,
		Mode:    "active",
		Backend: BackendStatus{Name: "fbink", State: "ready"},
	}
	if !statusIsSchemaValid(valid) {
		t.Fatalf("a valid document was rejected: %+v", valid)
	}
	for _, broken := range []StatusResponse{
		{Version: "v", Active: true, Mode: "inactive", Backend: BackendStatus{Name: "fbink", State: "ready"}},
		{Version: "v", Active: false, Mode: "active", Backend: BackendStatus{Name: "fbink", State: "ready"}},
		{Version: "v", Active: false, Mode: "", Backend: BackendStatus{Name: "fbink", State: "ready"}},
		{Version: "v", Active: true, Mode: "ACTIVE", Backend: BackendStatus{Name: "fbink", State: "ready"}},
	} {
		if statusIsSchemaValid(broken) {
			t.Fatalf("an inconsistent mode was accepted: %+v", broken)
		}
	}
}

// TestNonPutDisplayRequestsAreUnroutedRatherThan405 is deviations D2 and D3.
// putDisplayImage and putDisplayMarkdown declare no 405, so the only
// contract-allowed way to say "only PUT exists here" is the same path-level
// unrouted-resource answer the audit already accepted for unknown paths.
func TestNonPutDisplayRequestsAreUnroutedRatherThan405(t *testing.T) {
	_, token, handler, display, screen, native, state := newHarness(t)
	// OPTIONS and TRACE are included because they are the methods a client or an
	// intermediary sends without being asked to, and "put" because HTTP methods
	// are case-sensitive: none of them may be answered with the 405 that these
	// two operations do not declare.
	methods := []string{
		http.MethodGet,
		http.MethodPost,
		http.MethodPatch,
		http.MethodDelete,
		http.MethodHead,
		http.MethodOptions,
		http.MethodTrace,
		"put",
	}
	for _, target := range []string{"/v1/display/image", "/v1/display/markdown"} {
		for _, method := range methods {
			t.Run(method+" "+target, func(t *testing.T) {
				response := request(handler, method, target, "", nil, token)
				if response.Code != http.StatusNotFound {
					t.Fatalf("got %d %s", response.Code, response.Body.String())
				}
				if method != http.MethodHead && errorCode(t, response) != "not_found" {
					t.Fatalf("unexpected error code: %s", response.Body.String())
				}
			})
		}
	}
	if display.Calls != 0 || display.StatusCalls != 0 || screen.Calls != 0 || native.Calls != 0 {
		t.Fatalf("an unrouted request reached a port: display=%d status=%d screen=%d native=%d", display.Calls, display.StatusCalls, screen.Calls, native.Calls)
	}
	if snapshot := state.Snapshot("test-version", false, BackendStatus{}); snapshot.Current != nil || snapshot.LastError != nil {
		t.Fatalf("an unrouted request changed service state: %+v", snapshot)
	}
	// Authentication still runs first: an unauthenticated non-PUT request is
	// answered 401 and never learns whether the resource exists.
	unauthenticated := request(handler, http.MethodGet, "/v1/display/image", "", nil, nil)
	if unauthenticated.Code != http.StatusUnauthorized || errorCode(t, unauthenticated) != "unauthorized" {
		t.Fatalf("unauthenticated non-PUT returned %d", unauthenticated.Code)
	}
}

// TestDeclared405IsKeptWhereTheContractDeclaresIt is the other half of D2/D3:
// getStatus and postSystemExit declare 405, so those two must keep answering
// method_not_allowed. /healthz no longer exists, so it has no 405 to keep.
func TestDeclared405IsKeptWhereTheContractDeclaresIt(t *testing.T) {
	_, token, handler, _, _, _, _ := newHarness(t)
	cases := []struct {
		method string
		target string
		token  []byte
	}{
		{method: http.MethodPost, target: "/v1/status", token: token},
		{method: http.MethodPut, target: "/v1/status", token: token},
		{method: http.MethodGet, target: "/v1/system/exit", token: token},
		{method: http.MethodPut, target: "/v1/system/exit", token: token},
	}
	for _, test := range cases {
		t.Run(test.method+" "+test.target, func(t *testing.T) {
			response := request(handler, test.method, test.target, "", nil, test.token)
			if response.Code != http.StatusMethodNotAllowed || errorCode(t, response) != "method_not_allowed" {
				t.Fatalf("got %d %s", response.Code, response.Body.String())
			}
		})
	}
}

// TestEmptyMarkdownBodyIsRejectedBeforeAnyDisplay is deviation D5. The frozen
// contract marks the Markdown request body required, and the visible cost of not
// enforcing it is worse than a wrong status code: an empty body used to render
// an all-white full screen and be committed as the last successful frame, so a
// later restore would put a blank screen back on the panel.
func TestEmptyMarkdownBodyIsRejectedBeforeAnyDisplay(t *testing.T) {
	cases := []struct {
		name          string
		contentType   string
		unknownLength bool
	}{
		{name: "declared zero length", contentType: "text/markdown"},
		{name: "declared zero length with charset", contentType: "text/markdown; charset=utf-8"},
		{name: "unknown length and no bytes", contentType: "text/markdown", unknownLength: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, token, handler, display, screen, _, state := newHarness(t)
			req := httptest.NewRequest(http.MethodPut, "/v1/display/markdown", strings.NewReader(""))
			req.Header.Set("Content-Type", test.contentType)
			req.Header.Set("Authorization", "Bearer "+string(token))
			if test.unknownLength {
				req.ContentLength = -1
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusBadRequest || errorCode(t, response) != "invalid_request" {
				t.Fatalf("got %d %s", response.Code, response.Body.String())
			}
			if display.Calls != 0 || screen.Calls != 0 {
				t.Fatalf("an empty body reached a port: display=%d screen=%d", display.Calls, screen.Calls)
			}
			if snapshot := state.Snapshot("test-version", false, BackendStatus{}); snapshot.Current != nil || snapshot.LastError != nil {
				t.Fatalf("an empty body changed service state: %+v", snapshot)
			}
			if handler.busy.Load() {
				t.Fatal("the display lock was not released")
			}
		})
	}
}

// TestEmptyBodiesLeaveTheCommittedScreenAlone runs the same rejection against a
// real store and panel, which is where "nothing was displayed and nothing was
// committed" can actually be observed. It also pins the image endpoint's
// existing behaviour: a zero-length image body is rejected with the declared 422
// decode_failed, which the audit ruled is not a deviation and must not change.
func TestEmptyBodiesLeaveTheCommittedScreenAlone(t *testing.T) {
	screen := ScreenCapabilities{Width: 40, Height: 30}
	cases := []struct {
		name        string
		target      string
		contentType string
		wantStatus  int
		wantCode    string
	}{
		{name: "markdown", target: "/v1/display/markdown", contentType: "text/markdown; charset=utf-8", wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "image", target: "/v1/display/image", contentType: "image/png", wantStatus: http.StatusUnprocessableEntity, wantCode: "decode_failed"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			panel := &recordingPanel{}
			backend := &RenderingBackend{
				Store:  NewDisplayStore(directory),
				Panel:  panel,
				Limits: DefaultImageLimits(),
				Style:  DefaultMarkdownStyle(),
			}
			token := randomToken(t)
			state := NewStateStore(true)
			handler := NewHandler(DefaultConfig(), "test-version", NewAuthenticator(token), backend, &FakeNative{}, &FakeScreen{Capabilities: screen}, state)

			reject := func() {
				t.Helper()
				response := request(handler, http.MethodPut, test.target, test.contentType, strings.NewReader(""), token)
				if response.Code != test.wantStatus || errorCode(t, response) != test.wantCode {
					t.Fatalf("got %d %s", response.Code, response.Body.String())
				}
			}

			reject()
			if panel.calls() != 0 {
				t.Fatal("an empty body reached the panel")
			}
			assertNoCandidates(t, directory)
			if _, err := os.Stat(filepath.Join(directory, currentImageName)); !os.IsNotExist(err) {
				t.Fatalf("an empty body produced current.png: %v", err)
			}

			// Repeat with a screen already committed, so the assertion tells
			// "never written" apart from "written and then rolled back".
			seed := grayFixture(t, 20, 10, func(int, int) uint8 { return 0x20 })
			if _, err := backend.DisplayImage(context.Background(), "image/png", "contain", seed, screen); err != nil {
				t.Fatalf("seeding a committed screen failed: %v", err)
			}
			committed := readFile(t, filepath.Join(directory, currentImageName))
			shown := panel.calls()

			reject()
			if panel.calls() != shown {
				t.Fatal("an empty body reached the panel")
			}
			assertNoCandidates(t, directory)
			if !bytes.Equal(readFile(t, filepath.Join(directory, currentImageName)), committed) {
				t.Fatal("an empty body rewrote the committed screen")
			}
			if _, err := os.Stat(filepath.Join(directory, previousImageName)); !os.IsNotExist(err) {
				t.Fatalf("an empty body rotated the committed screen: %v", err)
			}
			if snapshot := state.Snapshot("test-version", false, BackendStatus{}); snapshot.Current != nil || snapshot.LastError != nil {
				t.Fatalf("an empty body changed service state: %+v", snapshot)
			}
		})
	}
}

// TestStatusReconcilesWithTheActivityRecord pins the gesture-exit path: the
// corner long-press is handled by the Guardian, which records inactive in
// activity.json, but the serve process keeps its own in-memory state. A status
// read must reconcile with the durable record, or /v1/status keeps claiming
// active forever after a gesture exit.
func TestStatusReconcilesWithTheActivityRecord(t *testing.T) {
	cfg, token, _, display, screen, native, state := newHarness(t)
	if !state.Snapshot("test-version", false, BackendStatus{}).Active {
		t.Fatal("the harness should start active")
	}
	cfg.ActivityPath = filepath.Join(t.TempDir(), "activity.json")
	handler := NewHandler(cfg, "test-version", NewAuthenticator(token), display, native, screen, state)
	if err := StoreActivity(cfg.ActivityPath, Activity{Active: false, Reason: exitReasonGesture, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	response := request(handler, http.MethodGet, "/v1/status", "", nil, token)
	if response.Code != http.StatusOK {
		t.Fatalf("status: %d", response.Code)
	}
	var body struct {
		Active bool   `json:"active"`
		Mode   string `json:"mode"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Active || body.Mode != "inactive" {
		t.Fatalf("status still claims active after the gesture exit was persisted: %+v", body)
	}
}

// TestMarkdownFontSizeParamScalesTheStyle pins the optional font_size query
// parameter: a valid value reaches the backend as a proportionally scaled
// style, the default matches the legacy behaviour, and anything malformed,
// out of range, duplicated or unknown is rejected before any port is touched.
func TestMarkdownFontSizeParamScalesTheStyle(t *testing.T) {
	_, token, handler, display, _, _, _ := newHarness(t)

	ok := request(handler, http.MethodPut, "/v1/display/markdown?font_size=32", "text/markdown", strings.NewReader("# Title"), token)
	if ok.Code != http.StatusOK {
		t.Fatalf("font_size=32 rejected: %d %s", ok.Code, ok.Body.String())
	}
	if display.LastStyle.BaseSize != 32 {
		t.Fatalf("the backend saw BaseSize %v, want 32", display.LastStyle.BaseSize)
	}

	legacy := request(handler, http.MethodPut, "/v1/display/markdown", "text/markdown", strings.NewReader("# Title"), token)
	if legacy.Code != http.StatusOK || display.LastStyle.BaseSize != DefaultMarkdownStyle().BaseSize {
		t.Fatalf("the default style changed: %v", display.LastStyle.BaseSize)
	}

	rejections := []string{
		"/v1/display/markdown?font_size=",
		"/v1/display/markdown?font_size=abc",
		"/v1/display/markdown?font_size=11",
		"/v1/display/markdown?font_size=73",
		"/v1/display/markdown?font_size=18&font_size=20",
		"/v1/display/markdown?font_size=18&fit=contain",
		"/v1/display/markdown?other=1",
	}
	for _, target := range rejections {
		t.Run(target, func(t *testing.T) {
			before := display.Calls
			response := request(handler, http.MethodPut, target, "text/markdown", strings.NewReader("# Title"), token)
			if response.Code != http.StatusBadRequest || errorCode(t, response) != "invalid_parameter" {
				t.Fatalf("got %d %s", response.Code, response.Body.String())
			}
			if display.Calls != before {
				t.Fatal("a rejected font_size reached the display port")
			}
		})
	}
}

func TestScaledMarkdownStyleKeepsProportions(t *testing.T) {
	base := DefaultMarkdownStyle()
	scaled := ScaledMarkdownStyle(36)
	if scaled.BaseSize != 36 {
		t.Fatalf("BaseSize = %v", scaled.BaseSize)
	}
	ratio := func(a, b float64) float64 { return a / b }
	if ratio(scaled.HeadingSizes[0], scaled.BaseSize) != ratio(base.HeadingSizes[0], base.BaseSize) {
		t.Fatal("heading ratio changed")
	}
	if scaled.Margin != base.Margin*2 || scaled.ParagraphGap != base.ParagraphGap*2 || scaled.IndentStep != base.IndentStep*2 || scaled.QuoteBar != base.QuoteBar*2 {
		t.Fatalf("pixel fields did not double: %+v", scaled)
	}
	if scaled.LineSpacing != base.LineSpacing {
		t.Fatal("line spacing is a ratio and must not scale")
	}
}

func TestNoConcurrentStatusMutationRace(t *testing.T) {
	_, token, handler, _, _, _, _ := newHarness(t)
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			response := request(handler, http.MethodGet, "/v1/status", "", nil, token)
			if response.Code != http.StatusOK {
				t.Errorf("status returned %d", response.Code)
			}
		}()
	}
	group.Wait()
}
