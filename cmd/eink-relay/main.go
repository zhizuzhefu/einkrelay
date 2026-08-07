package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
)

var version = "dev"

const (
	defaultImageMaxBytes    int64 = 10 * 1024 * 1024
	defaultMarkdownMaxBytes int64 = 1024 * 1024
	defaultStateDir               = "/var/local/einkrelay"
)

// forbiddenInputDevices carry power-button input on the validated PW4. They are
// never opened, read, grabbed or remapped, so the hardware long-press reboot
// stays available no matter what EInkRelay is doing.
var forbiddenInputDevices = []string{"event0", "event1"}

func isForbiddenInputDevice(path string) bool {
	base := filepath.Base(filepath.Clean(path))
	for _, name := range forbiddenInputDevices {
		if base == name {
			return true
		}
	}
	return false
}

type Config struct {
	ListenAddress      string
	ListenPort         int
	StateDir           string
	TokenPath          string
	GuardianSocket     string
	ActivityPath       string
	InputDevice        string
	FBInkPath          string
	ImageMaxBytes      int64
	MarkdownMaxBytes   int64
	ImageMaxDimension  int
	ImageMaxPixels     int64
	ReadTimeout        time.Duration
	TransactionTimeout time.Duration
	GestureTapWindow   time.Duration
	// LifecycleTimeout bounds entering and leaving exclusive mode, and the
	// Guardian control-socket round trip that carries a REST exit. Both
	// directions are now a couple of lipc property writes rather than a native
	// UI restart, so the old 45s allowance — sized for a ~25s unit restart plus
	// its repaint — no longer describes anything the device does.
	LifecycleTimeout time.Duration
}

func DefaultConfig() Config {
	return Config{
		ListenAddress:      "0.0.0.0",
		ListenPort:         8080,
		StateDir:           defaultStateDir,
		TokenPath:          defaultStateDir + "/token",
		GuardianSocket:     defaultStateDir + "/guardian.sock",
		ActivityPath:       defaultStateDir + "/activity.json",
		InputDevice:        "/dev/input/event2",
		FBInkPath:          "/mnt/us/einkrelay/bin/fbink",
		ImageMaxBytes:      defaultImageMaxBytes,
		MarkdownMaxBytes:   defaultMarkdownMaxBytes,
		ImageMaxDimension:  8192,
		ImageMaxPixels:     32000000,
		ReadTimeout:        15 * time.Second,
		TransactionTimeout: 60 * time.Second,
		GestureTapWindow:   time.Second,
		LifecycleTimeout:   10 * time.Second,
	}
}

func LoadConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := DefaultConfig()
	if value := getenv("EINKRELAY_LISTEN_ADDRESS"); value != "" {
		cfg.ListenAddress = value
	}
	if err := envInt(getenv, "EINKRELAY_LISTEN_PORT", &cfg.ListenPort); err != nil {
		return Config{}, err
	}
	// The state directory is resolved first so that relocating it moves the
	// token, socket and activity record together; explicit overrides below
	// still win.
	if value := getenv("EINKRELAY_STATE_DIR"); value != "" {
		cfg.StateDir = value
		cfg.TokenPath = filepath.Join(value, "token")
		cfg.GuardianSocket = filepath.Join(value, "guardian.sock")
		cfg.ActivityPath = filepath.Join(value, "activity.json")
	}
	if value := getenv("EINKRELAY_TOKEN_PATH"); value != "" {
		cfg.TokenPath = value
	}
	if value := getenv("EINKRELAY_GUARDIAN_SOCKET"); value != "" {
		cfg.GuardianSocket = value
	}
	if value := getenv("EINKRELAY_ACTIVITY_PATH"); value != "" {
		cfg.ActivityPath = value
	}
	if value := getenv("EINKRELAY_INPUT_DEVICE"); value != "" {
		cfg.InputDevice = value
	}
	if value := getenv("EINKRELAY_FBINK_PATH"); value != "" {
		cfg.FBInkPath = value
	}
	if err := envInt64(getenv, "EINKRELAY_IMAGE_MAX_BYTES", &cfg.ImageMaxBytes); err != nil {
		return Config{}, err
	}
	if err := envInt64(getenv, "EINKRELAY_MARKDOWN_MAX_BYTES", &cfg.MarkdownMaxBytes); err != nil {
		return Config{}, err
	}
	if err := envInt(getenv, "EINKRELAY_IMAGE_MAX_DIMENSION", &cfg.ImageMaxDimension); err != nil {
		return Config{}, err
	}
	if err := envInt64(getenv, "EINKRELAY_IMAGE_MAX_PIXELS", &cfg.ImageMaxPixels); err != nil {
		return Config{}, err
	}
	if err := envDuration(getenv, "EINKRELAY_READ_TIMEOUT", &cfg.ReadTimeout); err != nil {
		return Config{}, err
	}
	if err := envDuration(getenv, "EINKRELAY_TRANSACTION_TIMEOUT", &cfg.TransactionTimeout); err != nil {
		return Config{}, err
	}
	if err := envDuration(getenv, "EINKRELAY_GESTURE_TAP_WINDOW", &cfg.GestureTapWindow); err != nil {
		return Config{}, err
	}
	if err := envDuration(getenv, "EINKRELAY_LIFECYCLE_TIMEOUT", &cfg.LifecycleTimeout); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func envInt(getenv func(string) string, name string, target *int) error {
	value := getenv(name)
	if value == "" {
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return errors.New("invalid configuration")
	}
	*target = parsed
	return nil
}

func envInt64(getenv func(string) string, name string, target *int64) error {
	value := getenv(name)
	if value == "" {
		return nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return errors.New("invalid configuration")
	}
	*target = parsed
	return nil
}

func envDuration(getenv func(string) string, name string, target *time.Duration) error {
	value := getenv(name)
	if value == "" {
		return nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return errors.New("invalid configuration")
	}
	*target = parsed
	return nil
}

func (c Config) Validate() error {
	if c.ListenAddress == "" || c.ListenPort < 1 || c.ListenPort > 65535 || c.TokenPath == "" || c.FBInkPath == "" || c.ImageMaxBytes < 1 || c.MarkdownMaxBytes < 1 || c.ImageMaxDimension < 1 || c.ImageMaxPixels < 1 || c.ReadTimeout <= 0 || c.TransactionTimeout <= 0 {
		return errors.New("invalid configuration")
	}
	if c.StateDir == "" || c.GuardianSocket == "" || c.ActivityPath == "" || c.InputDevice == "" || c.GestureTapWindow <= 0 || c.LifecycleTimeout <= 0 {
		return errors.New("invalid configuration")
	}
	// Refusing the power-button devices here means no later code path, and no
	// operator environment variable, can point the gesture reader at them.
	if isForbiddenInputDevice(c.InputDevice) {
		return errors.New("invalid configuration")
	}
	return nil
}

func (c Config) Address() string {
	return net.JoinHostPort(c.ListenAddress, strconv.Itoa(c.ListenPort))
}

type BackendStatus struct {
	Name    string  `json:"name"`
	State   string  `json:"state"`
	Version *string `json:"version"`
}

type ScreenCapabilities struct {
	Width  int
	Height int
}

type DisplayResult struct {
	SHA256      string
	DisplayedAt time.Time
}

type InputEvent struct {
	Type  uint16
	Code  uint16
	Value int32
	At    time.Time
}

type DisplayBackend interface {
	DisplayImage(context.Context, string, string, []byte, ScreenCapabilities) (DisplayResult, error)
	DisplayMarkdown(context.Context, []byte, ScreenCapabilities, MarkdownStyle) (DisplayResult, error)
	Status(context.Context) BackendStatus
}

type InputSource interface {
	Next(context.Context) (InputEvent, error)
	Close() error
}

type NativeController interface {
	ExitExclusive(context.Context) error
}

type ScreenProbe interface {
	Probe(context.Context) (ScreenCapabilities, error)
}

var ErrUnprocessable = errors.New("unprocessable content")

type FakeDisplay struct {
	mu               sync.Mutex
	Backend          BackendStatus
	Err              error
	Entered          chan struct{}
	Release          chan struct{}
	BlockUntilCancel bool
	Calls            int
	StatusCalls      int
	Active           int
	MaxActive        int
	LastFit          string
	LastStyle        MarkdownStyle
	enterOnce        sync.Once
}

func (f *FakeDisplay) DisplayImage(ctx context.Context, mediaType, fit string, body []byte, screen ScreenCapabilities) (DisplayResult, error) {
	f.mu.Lock()
	f.LastFit = fit
	f.mu.Unlock()
	return f.display(ctx, body)
}

func (f *FakeDisplay) DisplayMarkdown(ctx context.Context, body []byte, screen ScreenCapabilities, style MarkdownStyle) (DisplayResult, error) {
	f.mu.Lock()
	f.LastStyle = style
	f.mu.Unlock()
	return f.display(ctx, body)
}

func (f *FakeDisplay) display(ctx context.Context, body []byte) (DisplayResult, error) {
	f.mu.Lock()
	f.Calls++
	f.Active++
	if f.Active > f.MaxActive {
		f.MaxActive = f.Active
	}
	configuredErr := f.Err
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.Active--
		f.mu.Unlock()
	}()
	if f.Entered != nil {
		f.enterOnce.Do(func() { close(f.Entered) })
	}
	if f.BlockUntilCancel {
		<-ctx.Done()
		return DisplayResult{}, ctx.Err()
	}
	if f.Release != nil {
		select {
		case <-f.Release:
		case <-ctx.Done():
			return DisplayResult{}, ctx.Err()
		}
	}
	if configuredErr != nil {
		return DisplayResult{}, configuredErr
	}
	digest := sha256.Sum256(body)
	return DisplayResult{SHA256: hex.EncodeToString(digest[:]), DisplayedAt: time.Now().UTC()}, nil
}

func (f *FakeDisplay) Status(context.Context) BackendStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.StatusCalls++
	status := f.Backend
	if status.Name == "" {
		status = BackendStatus{Name: "fake", State: "ready"}
	}
	return status
}

type FakeScreen struct {
	mu           sync.Mutex
	Capabilities ScreenCapabilities
	Err          error
	Calls        int
}

func (f *FakeScreen) Probe(context.Context) (ScreenCapabilities, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	return f.Capabilities, f.Err
}

type FakeNative struct {
	mu    sync.Mutex
	Err   error
	Calls int
}

func (f *FakeNative) ExitExclusive(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	return f.Err
}

type FakeInput struct {
	Events <-chan InputEvent
}

func (f *FakeInput) Next(ctx context.Context) (InputEvent, error) {
	select {
	case event, ok := <-f.Events:
		if !ok {
			return InputEvent{}, io.EOF
		}
		return event, nil
	case <-ctx.Done():
		return InputEvent{}, ctx.Err()
	}
}

func (*FakeInput) Close() error { return nil }

var _ DisplayBackend = (*FakeDisplay)(nil)
var _ ScreenProbe = (*FakeScreen)(nil)
var _ NativeController = (*FakeNative)(nil)
var _ InputSource = (*FakeInput)(nil)

type CurrentDisplay struct {
	SHA256      string    `json:"sha256"`
	DisplayedAt time.Time `json:"displayed_at"`
}

type SafeError struct {
	Code    string    `json:"code"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// ScreenSize is the visible panel geometry. It is reported because a caller
// otherwise has no way to size content correctly: fitting an image means
// knowing the panel is 1072x1448 on this device, and a caller that guesses is
// exactly how a request succeeds while the screen shows nothing useful.
type ScreenSize struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type StatusResponse struct {
	Version   string          `json:"version"`
	Active    bool            `json:"active"`
	Mode      string          `json:"mode"`
	Busy      bool            `json:"busy"`
	Screen    *ScreenSize     `json:"screen"`
	Current   *CurrentDisplay `json:"current"`
	Backend   BackendStatus   `json:"backend"`
	LastError *SafeError      `json:"last_error"`
}

type StateStore struct {
	mu        sync.RWMutex
	active    bool
	current   *CurrentDisplay
	lastError *SafeError
	// persistent is a condition that remains true until somebody repairs the
	// device itself, such as pinned fonts that do not verify. A successful
	// display proves nothing about it — an image renders perfectly well while
	// Markdown stays broken — so it has to survive Commit and keep being
	// reported until the condition has been repaired and explicitly cleared.
	persistent *SafeError
}

func NewStateStore(active bool) *StateStore { return &StateStore{active: active} }

func (s *StateStore) Snapshot(version string, busy bool, backend BackendStatus) StatusResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()
	mode := "inactive"
	if s.active {
		mode = "active"
	}
	response := StatusResponse{Version: version, Active: s.active, Mode: mode, Busy: busy, Backend: backend}
	if s.current != nil {
		copy := *s.current
		response.Current = &copy
	}
	reported := s.lastError
	if reported == nil {
		reported = s.persistent
	}
	if reported != nil {
		copy := *reported
		response.LastError = &copy
	}
	return response
}

// Commit clears the transient failure that a successful display has just
// disproved. Snapshot continues to report a persistent condition when there is
// one: the screen going up says nothing about a fault that only a repair on the
// device can end.
func (s *StateStore) Commit(result DisplayResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = &CurrentDisplay{SHA256: result.SHA256, DisplayedAt: result.DisplayedAt.UTC()}
	s.lastError = nil
}

func (s *StateStore) SetInactive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = false
}

// RecordError overwrites whatever was last reported with the newer transient
// failure, because the fresher fact is the more useful one to show. It leaves
// the persistent condition alone: that one has not been disproved either, and
// the next Commit brings it back into view.
func (s *StateStore) RecordError(code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError = &SafeError{Code: code, Message: message, At: time.Now().UTC()}
}

// SetPersistentError records a condition that only a repair on the device can
// end. Snapshot reports it whenever no newer transient failure is present.
func (s *StateStore) SetPersistentError(code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recorded := SafeError{Code: code, Message: message, At: time.Now().UTC()}
	s.persistent = &recorded
}

// ClearPersistentError is the explicit recovery edge. Callers must only use it
// after re-verifying the underlying condition (for example after loading the
// pinned fonts). A newer transient failure remains independently reportable.
func (s *StateStore) ClearPersistentError() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persistent = nil
}

type Authenticator struct {
	expected [sha256.Size]byte
}

func NewAuthenticator(token []byte) *Authenticator {
	return &Authenticator{expected: sha256.Sum256(token)}
}

func LoadToken(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() < 32 || info.Size() > 512 {
		return nil, errors.New("token validation failed")
	}
	value, err := os.ReadFile(path)
	if err != nil || len(value) < 32 || len(value) > 512 {
		return nil, errors.New("token validation failed")
	}
	for _, b := range value {
		if b < 0x21 || b > 0x7e {
			return nil, errors.New("token validation failed")
		}
	}
	return value, nil
}

func (a *Authenticator) Allowed(header http.Header) bool {
	values := header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return false
	}
	provided := strings.TrimPrefix(values[0], "Bearer ")
	if provided == "" || strings.ContainsAny(provided, " \t\r\n,") {
		return false
	}
	digest := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(a.expected[:], digest[:]) == 1
}

type APIError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type Handler struct {
	cfg     Config
	version string
	auth    *Authenticator
	display DisplayBackend
	native  NativeController
	screen  ScreenProbe
	state   *StateStore
	gate    chan struct{}
	busy    atomic.Bool
}

func NewHandler(cfg Config, version string, auth *Authenticator, display DisplayBackend, native NativeController, screen ScreenProbe, state *StateStore) *Handler {
	return &Handler{cfg: cfg, version: version, auth: auth, display: display, native: native, screen: screen, state: state, gate: make(chan struct{}, 1)}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/") {
		if !h.auth.Allowed(r.Header) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			h.writeError(w, http.StatusUnauthorized, "unauthorized", "authentication is required")
			return
		}
		switch r.URL.Path {
		case "/v1/status":
			if r.Method != http.MethodGet {
				h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
				return
			}
			h.writeStatus(w)
		// putDisplayImage and putDisplayMarkdown declare no 405, so a non-PUT
		// request is answered the same way an unrouted path is: PUT is the only
		// resource that exists here, and anything else is simply not found. That
		// is the path-level behaviour the contract already tolerates for unknown
		// paths, and it keeps these two operations inside their declared
		// response sets.
		case "/v1/display/image":
			if r.Method != http.MethodPut {
				h.writeError(w, http.StatusNotFound, "not_found", "resource was not found")
				return
			}
			h.handleImage(w, r)
		case "/v1/display/markdown":
			if r.Method != http.MethodPut {
				h.writeError(w, http.StatusNotFound, "not_found", "resource was not found")
				return
			}
			h.handleMarkdown(w, r)
		case "/v1/system/exit":
			if r.Method != http.MethodPost {
				h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
				return
			}
			h.handleExit(w, r)
		default:
			h.writeError(w, http.StatusNotFound, "not_found", "resource was not found")
		}
		return
	}
	h.writeError(w, http.StatusNotFound, "not_found", "resource was not found")
}

func (h *Handler) acquire(w http.ResponseWriter) bool {
	select {
	case h.gate <- struct{}{}:
		h.busy.Store(true)
		return true
	default:
		h.writeError(w, http.StatusConflict, "display_busy", "another display transaction is active")
		return false
	}
}

func (h *Handler) release() {
	h.busy.Store(false)
	<-h.gate
}

func (h *Handler) handleImage(w http.ResponseWriter, r *http.Request) {
	if !h.acquire(w) {
		return
	}
	defer h.release()
	fit, ok := h.fit(w, r)
	if !ok || !h.identityEncoding(w, r) {
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mediaType != "image/png" && mediaType != "image/jpeg") {
		h.writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "media type is not supported")
		return
	}
	body, ok := h.readBody(w, r, h.cfg.ImageMaxBytes)
	if !ok {
		return
	}
	h.displayBody(w, r, func(ctx context.Context, screen ScreenCapabilities) (DisplayResult, error) {
		return h.display.DisplayImage(ctx, mediaType, fit, body, screen)
	})
}

func (h *Handler) handleMarkdown(w http.ResponseWriter, r *http.Request) {
	if !h.acquire(w) {
		return
	}
	defer h.release()
	style, ok := h.markdownStyle(w, r)
	if !ok || !h.identityEncoding(w, r) {
		return
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/markdown" || len(params) > 1 {
		h.writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "media type is not supported")
		return
	}
	if charset, present := params["charset"]; present && !strings.EqualFold(charset, "utf-8") {
		h.writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "media type is not supported")
		return
	}
	body, ok := h.readBody(w, r, h.cfg.MarkdownMaxBytes)
	if !ok {
		return
	}
	// The contract marks this request body required. Enforcing it here, before
	// any render, any display-port call and any commit, is also what stops a
	// zero-length body from rasterising to an all-white full screen and being
	// committed as the last successful frame — which a later restore would then
	// put back on the panel.
	if len(body) == 0 {
		h.writeError(w, http.StatusBadRequest, "invalid_request", "request body is required")
		return
	}
	if !utf8.Valid(body) {
		h.writeError(w, http.StatusBadRequest, "invalid_encoding", "request body must be valid UTF-8")
		return
	}
	h.displayBody(w, r, func(ctx context.Context, screen ScreenCapabilities) (DisplayResult, error) {
		return h.display.DisplayMarkdown(ctx, body, screen, style)
	})
}

// markdownStyle parses the optional font_size query parameter. Absent means
// DefaultMarkdownStyle, which is exactly the pre-parameter behaviour. The
// value must be a single integer point size inside [minFontSize, maxFontSize];
// anything else — empty, non-numeric, out of range, duplicated, or any other
// parameter — is rejected before a port is touched.
func (h *Handler) markdownStyle(w http.ResponseWriter, r *http.Request) (MarkdownStyle, bool) {
	query, err := r.URL.Query(), error(nil)
	if strings.Contains(r.URL.RawQuery, ";") {
		err = errors.New("invalid query")
	}
	if err != nil || len(query) > 1 {
		h.writeError(w, http.StatusBadRequest, "invalid_parameter", "request parameters are invalid")
		return MarkdownStyle{}, false
	}
	values, exists := query["font_size"]
	if len(query) == 1 && !exists {
		// font_size is the only declared parameter on this endpoint.
		h.writeError(w, http.StatusBadRequest, "invalid_parameter", "request parameters are invalid")
		return MarkdownStyle{}, false
	}
	if !exists {
		return DefaultMarkdownStyle(), true
	}
	if len(values) != 1 {
		h.writeError(w, http.StatusBadRequest, "invalid_parameter", "request parameters are invalid")
		return MarkdownStyle{}, false
	}
	size, parseErr := strconv.Atoi(values[0])
	if parseErr != nil || size < minFontSize || size > maxFontSize {
		h.writeError(w, http.StatusBadRequest, "invalid_parameter", "request parameters are invalid")
		return MarkdownStyle{}, false
	}
	return ScaledMarkdownStyle(float64(size)), true
}

func (h *Handler) fit(w http.ResponseWriter, r *http.Request) (string, bool) {
	query, err := r.URL.Query(), error(nil)
	if strings.Contains(r.URL.RawQuery, ";") {
		err = errors.New("invalid query")
	}
	if err != nil || len(query) > 1 {
		h.writeError(w, http.StatusBadRequest, "invalid_parameter", "request parameters are invalid")
		return "", false
	}
	values, exists := query["fit"]
	if !exists {
		return "contain", true
	}
	if len(values) != 1 || (values[0] != "contain" && values[0] != "cover") {
		h.writeError(w, http.StatusBadRequest, "invalid_parameter", "request parameters are invalid")
		return "", false
	}
	return values[0], true
}

func (h *Handler) identityEncoding(w http.ResponseWriter, r *http.Request) bool {
	values := r.Header.Values("Content-Encoding")
	if len(values) == 0 {
		return true
	}
	if len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), "identity") {
		return true
	}
	h.writeError(w, http.StatusUnsupportedMediaType, "unsupported_content_encoding", "content encoding is not supported")
	return false
}

func (h *Handler) readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	if r.ContentLength > limit {
		h.writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body is too large")
		return nil, false
	}
	reader := http.MaxBytesReader(w, r.Body, limit)
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err == nil {
		return body, true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		h.writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body is too large")
		return nil, false
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		h.writeError(w, http.StatusRequestTimeout, "request_timeout", "request body read timed out")
		return nil, false
	}
	h.writeError(w, http.StatusBadRequest, "invalid_request", "request is invalid")
	return nil, false
}

func (h *Handler) displayBody(w http.ResponseWriter, r *http.Request, operation func(context.Context, ScreenCapabilities) (DisplayResult, error)) {
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.TransactionTimeout)
	defer cancel()
	screen, err := h.screen.Probe(ctx)
	if err != nil || screen.Width < 1 || screen.Height < 1 {
		h.recordAndWrite(w, http.StatusInternalServerError, "display_failed", "display transaction failed")
		return
	}
	result, err := operation(ctx, screen)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			h.recordAndWrite(w, http.StatusGatewayTimeout, "transaction_timeout", "display transaction timed out")
			return
		}
		h.writeDisplayFailure(w, err)
		return
	}
	digest, err := hex.DecodeString(result.SHA256)
	if err != nil || len(digest) != sha256.Size || result.DisplayedAt.IsZero() {
		h.recordAndWrite(w, http.StatusInternalServerError, "internal_error", "display transaction failed")
		return
	}
	h.state.Commit(result)
	h.writeStatus(w)
}

// writeDisplayFailure maps the render and persistence sentinels onto the frozen
// error codes. Every branch here is a path on which nothing was displayed and
// nothing was promoted, so the last successful screen is still on the panel and
// current/previous are untouched.
//
// Client-content failures (4xx) are answered but not recorded; service-side
// failures (5xx) are also recorded, because those are what /v1/status exists to
// surface. The recorded text is fixed wording: no path, no digest, no command
// output and no request content ever reaches it.
func (h *Handler) writeDisplayFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrImageDimensions):
		h.writeError(w, http.StatusRequestEntityTooLarge, "image_dimensions_exceeded", "declared image dimensions or decode budget exceed the configured limits")
	case errors.Is(err, ErrDecodeFailed):
		h.writeError(w, http.StatusUnprocessableEntity, "decode_failed", "content could not be decoded")
	case errors.Is(err, ErrUnprocessable), errors.Is(err, ErrMissingGlyph), errors.Is(err, ErrMarkdownRender):
		h.writeError(w, http.StatusUnprocessableEntity, "render_failed", "content could not be processed")
	case errors.Is(err, ErrPersistence):
		h.recordAndWrite(w, http.StatusInternalServerError, "persistence_failed", "the display transaction could not be persisted")
	case errors.Is(err, ErrFontMissing), errors.Is(err, ErrFontDigest), errors.Is(err, ErrFontManifest):
		// Fail closed rather than render notdef boxes that would be committed as
		// the last successful screen.
		h.recordAndWrite(w, http.StatusInternalServerError, "display_failed", "the pinned fonts are unavailable")
	default:
		h.recordAndWrite(w, http.StatusInternalServerError, "display_failed", "display transaction failed")
	}
}

// handleExit hands the recovery over to the Guardian rather than performing it
// here. That is what keeps a repeated exit idempotent and, more importantly,
// keeps the single recovery implementation outside the process that renders.
func (h *Handler) handleExit(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		h.writeError(w, http.StatusBadRequest, "invalid_parameter", "request parameters are invalid")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(body) != 0 {
		h.writeError(w, http.StatusBadRequest, "invalid_request", "request is invalid")
		return
	}
	if err := h.native.ExitExclusive(r.Context()); err != nil {
		h.recordAndWrite(w, http.StatusInternalServerError, "lifecycle_failed", "native UI recovery failed")
		return
	}
	h.state.SetInactive()
	h.writeStatus(w)
}

// isFrozenErrorCode reports whether code is one of the nineteen values of the
// frozen ErrorCode enum. A status document whose last_error.code is not one of
// them is not a document the contract admits, however the value got there.
func isFrozenErrorCode(code string) bool {
	switch code {
	case "invalid_request",
		"invalid_parameter",
		"invalid_encoding",
		"unauthorized",
		"not_found",
		"method_not_allowed",
		"request_timeout",
		"display_busy",
		"payload_too_large",
		"image_dimensions_exceeded",
		"unsupported_media_type",
		"unsupported_content_encoding",
		"decode_failed",
		"render_failed",
		"display_failed",
		"persistence_failed",
		"lifecycle_failed",
		"internal_error",
		"transaction_timeout":
		return true
	}
	return false
}

// statusDigestPattern is the frozen CurrentDisplay.sha256 pattern, spelled the
// way the contract spells it.
var statusDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// statusIsSchemaValid answers whether the snapshot is a document the frozen
// Status schema accepts. The constraints it enforces are the ones a caller can
// actually be handed a violation of: a version that is not minLength 1, a mode
// that contradicts active, a backend name or state the BackendStatus schema
// rejects, a digest outside the sha256 pattern, and a last_error code outside
// the frozen enum.
//
// The check exists because these values arrive from ports — a display backend
// reports its own name and state — and a schema-invalid 200 is a contract
// violation that a client cannot defend itself against, whereas the declared
// 500 is one it is already required to handle.
func statusIsSchemaValid(status StatusResponse) bool {
	if status.Version == "" {
		return false
	}
	mode := "inactive"
	if status.Active {
		mode = "active"
	}
	if status.Mode != mode {
		return false
	}
	if status.Backend.Name == "" {
		return false
	}
	switch status.Backend.State {
	case "ready", "unavailable", "error":
	default:
		return false
	}
	if status.Screen != nil && (status.Screen.Width < 1 || status.Screen.Height < 1) {
		return false
	}
	if status.Current != nil && !statusDigestPattern.MatchString(status.Current.SHA256) {
		return false
	}
	if status.LastError != nil && !isFrozenErrorCode(status.LastError.Code) {
		return false
	}
	return true
}

// writeStatus is the 200 path of getStatus, of both display endpoints and of
// postSystemExit. The snapshot is assembled, checked against the frozen Status
// schema and serialised into a buffer before the status line is written, so a
// document that would violate the contract is answered with the declared 500
// InternalError instead of being emitted as a schema-invalid 200. Every one of
// those four operations declares 500, so this adds no undeclared response.
func (h *Handler) writeStatus(w http.ResponseWriter) {
	h.reconcileActivity()
	snapshot := h.state.Snapshot(h.version, h.busy.Load(), h.display.Status(context.Background()))
	// Probed rather than cached: the geometry is what the next request will be
	// rendered against, so reporting a remembered value would be reporting
	// something the caller cannot rely on. A probe failure is reported as a
	// null screen rather than as an error, because /v1/status has to stay
	// reachable precisely when the device is unhealthy.
	if screen, err := h.screen.Probe(context.Background()); err == nil && screen.Width > 0 && screen.Height > 0 {
		snapshot.Screen = &ScreenSize{Width: screen.Width, Height: screen.Height}
	}
	if !statusIsSchemaValid(snapshot) {
		h.writeError(w, http.StatusInternalServerError, "internal_error", "the service status could not be produced")
		return
	}
	if err := h.writeJSON(w, http.StatusOK, snapshot); err != nil {
		// Nothing has been written yet, so the failure is still answerable.
		h.writeError(w, http.StatusInternalServerError, "internal_error", "the service status could not be produced")
	}
}

// reconcileActivity folds the durable activity record into the in-memory
// state before a status snapshot. The gesture exit is executed by the Guardian
// and only ever touches activity.json; without this read-back the serve
// process would report active forever after a corner long-press. The sync is
// one-way (inactive only): a persisted active intent is acted on at startup by
// runServe, never by a status poll. An unreadable or unparseable record is
// skipped rather than trusted — LoadActivity alone fails closed to inactive,
// which is right for entry decisions but wrong for reporting.
func (h *Handler) reconcileActivity() {
	if h.cfg.ActivityPath == "" {
		return
	}
	raw, err := os.ReadFile(h.cfg.ActivityPath)
	if err != nil {
		return
	}
	var activity Activity
	if json.Unmarshal(raw, &activity) != nil {
		return
	}
	if !activity.Active {
		h.state.SetInactive()
	}
}

func (h *Handler) recordAndWrite(w http.ResponseWriter, status int, code, message string) {
	h.state.RecordError(code, message)
	h.writeError(w, status, code, message)
}

func (h *Handler) writeError(w http.ResponseWriter, status int, code, message string) {
	response := APIError{}
	response.Error.Code = code
	response.Error.Message = message
	// Two strings cannot fail to marshal, so there is no second failure to
	// answer here; an error response is the last thing this layer can write.
	_ = h.writeJSON(w, status, response)
}

// writeJSON serialises into a buffer before touching the response, so that a
// marshal failure is reported to the caller while the status line is still
// unwritten. json.NewEncoder(w).Encode would have committed a 200 first and
// then produced a truncated body under it.
func (h *Handler) writeJSON(w http.ResponseWriter, status int, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(encoded, '\n'))
	return nil
}

func NewHTTPServer(cfg Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.Address(),
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		MaxHeaderBytes:    16 * 1024,
	}
}

func CheckPreflight(ctx context.Context, path, expectedSHA256 string) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return errors.New("preflight failed")
	}
	if expectedSHA256 != "" {
		expected, err := hex.DecodeString(expectedSHA256)
		if err != nil || len(expected) != sha256.Size {
			return errors.New("preflight failed")
		}
		file, err := os.Open(path)
		if err != nil {
			return errors.New("preflight failed")
		}
		hasher := sha256.New()
		_, copyErr := io.Copy(hasher, file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil || subtle.ConstantTimeCompare(expected, hasher.Sum(nil)) != 1 {
			return errors.New("preflight failed")
		}
	}
	// Upstream FBInk has no `--version` flag (it exits 255 on unknown options);
	// `--help` is the only invocation guaranteed to succeed on a working binary.
	command := exec.CommandContext(ctx, path, "--help")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return errors.New("preflight failed")
	}
	return nil
}

func runCLI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "version":
		if len(args) != 1 {
			printUsage(stderr)
			return 2
		}
		fmt.Fprintln(stdout, version)
		return 0
	case "guard":
		if len(args) != 1 {
			printUsage(stderr)
			return 2
		}
		return runGuard(ctx, stdout, stderr)
	case "resume":
		if len(args) != 1 {
			printUsage(stderr)
			return 2
		}
		return runResume(ctx, stdout, stderr)
	case "fonts":
		// Installation is a separate, explicitly invoked step: the service never
		// downloads anything on a render path.
		return runFonts(ctx, args[1:], stdout, stderr)
	case "preflight":
		cfg, err := LoadConfigFromEnv(os.Getenv)
		if err != nil {
			fmt.Fprintln(stderr, "configuration is invalid")
			return 2
		}
		flags := flag.NewFlagSet("preflight", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		fbink := flags.String("fbink", cfg.FBInkPath, "FBInk executable")
		checksum := flags.String("sha256", "", "expected SHA-256")
		if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
			printUsage(stderr)
			return 2
		}
		if err := CheckPreflight(ctx, *fbink, *checksum); err != nil {
			fmt.Fprintln(stderr, "preflight failed")
			return 1
		}
		fmt.Fprintln(stdout, "preflight ok")
		return 0
	case "serve":
		if len(args) != 1 {
			printUsage(stderr)
			return 2
		}
		return runServe(ctx, stdout, stderr)
	default:
		printUsage(stderr)
		return 2
	}
}

func runServe(ctx context.Context, stdout, stderr io.Writer) int {
	cfg, err := LoadConfigFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(stderr, "configuration is invalid")
		return 2
	}
	limits, err := LoadImageLimitsFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(stderr, "configuration is invalid")
		return 2
	}
	token, err := LoadToken(cfg.TokenPath)
	if err != nil {
		fmt.Fprintln(stderr, "token validation failed")
		return 1
	}
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		fmt.Fprintln(stderr, "the state directory is not usable")
		return 1
	}
	// The panel geometry is read from the framebuffer at request time rather
	// than compiled in, so the same binary adapts to whatever device it runs on.
	screen := &SysfsScreenProbe{}
	panel := &FBInkPanel{Path: cfg.FBInkPath}
	panel.Probe(ctx)
	backend := &RenderingBackend{
		Store:  NewDisplayStore(cfg.StateDir),
		Panel:  panel,
		Limits: limits,
		Style:  DefaultMarkdownStyle(),
	}
	// A font problem disables Markdown rendering and shows up in /v1/status. It
	// never degrades into a page of notdef boxes, and it never stops the
	// service: /v1/status and /v1/system/exit have to stay reachable while it
	// is being repaired.
	library, fontErr := loadRuntimeFonts(os.Getenv)
	if fontErr == nil {
		defer library.Close()
	} else {
		fmt.Fprintln(stderr, "fonts: "+fontStatusMessage(fontErr)+"; run `eink-relay fonts ensure`")
		// The device this runs on is frequently the one that cannot reach the
		// network, so the warning is useless without the route that does not
		// need it: stage the files from a host and verify them in place.
		fmt.Fprintln(stderr, fontOfflinePreseedHint)
	}
	backend.Fonts = library
	backend.FontErr = fontErr

	activity := LoadActivity(cfg.ActivityPath)
	state := NewStateStore(activity.Active)
	if fontErr != nil {
		// Recorded as persistent rather than transient: the very next successful
		// image would otherwise clear it, leaving a device that reports a clean
		// status while every Markdown request keeps failing. It is recorded before
		// the restore so that a failed restore, which is the fresher fact, still
		// shadows it until the next successful display brings it back.
		state.SetPersistentError("display_failed", fontStatusMessage(fontErr))
	}
	if activity.Active {
		// The device was in exclusive mode when it stopped, so the last
		// successful screen is put back before the listener opens.
		restoreLastScreenAtStartup(ctx, backend, screen, state, stderr)
	}
	// Exit is delegated to the Guardian over its socket. When the Guardian is
	// absent the call fails fast instead of falling back to a second, local
	// recovery implementation that could race the real one.
	native := &GuardianClient{Path: cfg.GuardianSocket, Timeout: cfg.LifecycleTimeout}
	handler := NewHandler(cfg, version, NewAuthenticator(token), backend, native, screen, state)
	server := NewHTTPServer(cfg, handler)
	errorsChannel := make(chan error, 1)
	go func() { errorsChannel <- server.ListenAndServe() }()
	select {
	case err := <-errorsChannel:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, "server stopped unexpectedly")
			return 1
		}
		return 0
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			fmt.Fprintln(stderr, "server shutdown failed")
			return 1
		}
		err := <-errorsChannel
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, "server stopped unexpectedly")
			return 1
		}
		return 0
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: eink-relay serve|guard|version|preflight|resume|fonts")
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(runCLI(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
