package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ErrForbiddenInputDevice is returned when anything tries to open a node that
// carries power-button input. Refusing it at the opener, not only at
// configuration validation, means no code path can reach event0/event1 by
// mistake: the hardware long-press reboot has to keep working.
var ErrForbiddenInputDevice = errors.New("input device is forbidden")

// ErrInputEvent marks a frame that does not decode as a struct input_event.
var ErrInputEvent = errors.New("input event is malformed")

// eventLongSize matches the C long in the kernel's struct timeval: 4 bytes on
// the 32-bit ARMv7 target and 8 on a 64-bit development host. Deriving it from
// the word size keeps one pure-Go decoder correct on both without cgo.
const eventLongSize = bits.UintSize / 8

// inputEventSize is sizeof(struct input_event): two longs of timestamp followed
// by u16 type, u16 code and s32 value.
const inputEventSize = 2*eventLongSize + 8

const (
	evSyn = 0x00
	evKey = 0x01
	evAbs = 0x03

	synReport  = 0x00
	synDropped = 0x03
	btnTouch   = 0x14a

	absX            = 0x00
	absY            = 0x01
	absMTSlot       = 0x2f
	absMTPositionX  = 0x35
	absMTPositionY  = 0x36
	absMTTrackingID = 0x39
)

func decodeLong(raw []byte) int64 {
	if len(raw) == 8 {
		return int64(binary.LittleEndian.Uint64(raw))
	}
	return int64(int32(binary.LittleEndian.Uint32(raw)))
}

// decodeInputEvent reads one little-endian struct input_event. The kernel
// timestamp is carried through instead of being replaced by the local clock,
// which is what lets a recorded event stream drive the state machine
// deterministically in a test.
func decodeInputEvent(raw []byte) (InputEvent, error) {
	// Accept both ABI encodings so recorded PW4 (32-bit, 16-byte) streams can
	// be replayed on a 64-bit development host. Live reads still use the native
	// inputEventSize selected above.
	if len(raw) != 16 && len(raw) != 24 {
		return InputEvent{}, ErrInputEvent
	}
	longSize := (len(raw) - 8) / 2
	seconds := decodeLong(raw[:longSize])
	microseconds := decodeLong(raw[longSize : 2*longSize])
	if microseconds < 0 || microseconds > 999999 {
		return InputEvent{}, ErrInputEvent
	}
	rest := raw[2*longSize:]
	return InputEvent{
		Type:  binary.LittleEndian.Uint16(rest[0:2]),
		Code:  binary.LittleEndian.Uint16(rest[2:4]),
		Value: int32(binary.LittleEndian.Uint32(rest[4:8])),
		At:    time.Unix(seconds, microseconds*1000).UTC(),
	}, nil
}

// EvdevSource reads the touch node read-only. It issues EVIOCGRAB only while
// the panel is actually covered by this service (see Grab); the rest of the
// time events stay available to the native interface, and the kernel releases
// the grab on close so a dead Guardian cannot keep the touchscreen.
type EvdevSource struct {
	file *os.File
	buf  []byte
}

func OpenEvdev(path string) (*EvdevSource, error) {
	// The touchscreen is deliberately pinned to the validated PW4 node.  Do
	// not follow links: a seemingly harmless event2 symlink can otherwise be
	// redirected to either power-button node after configuration validation.
	if filepath.Base(filepath.Clean(path)) != "event2" || isForbiddenInputDevice(path) {
		return nil, ErrForbiddenInputDevice
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeCharDevice == 0 {
		return nil, ErrForbiddenInputDevice
	}
	// O_NOFOLLOW closes the Lstat-to-open race as well as rejecting the common
	// static link case above.  Checking after a normal os.OpenFile is too late:
	// it has already opened event0/event1 if an attacker swapped the path.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	opened, err := file.Stat()
	initial, initialOK := info.Sys().(*syscall.Stat_t)
	actual, actualOK := opened.Sys().(*syscall.Stat_t)
	if err != nil || opened.Mode()&os.ModeCharDevice == 0 || !initialOK || !actualOK || initial.Rdev != actual.Rdev {
		_ = file.Close()
		return nil, ErrForbiddenInputDevice
	}
	return &EvdevSource{file: file, buf: make([]byte, inputEventSize)}, nil
}

// eviocgrab is EVIOCGRAB, _IOW('E', 0x90, int). A non-zero argument takes the
// device exclusively; zero releases it.
const eviocgrab = 0x40044590

// Grab takes or releases exclusive ownership of the touch stream.
//
// The old design never grabbed, on the grounds that the native interface had
// to keep receiving events. That was correct while exclusive mode worked by
// stopping the interface outright — there was nothing left to receive them.
// Now that the interface stays alive underneath a covered panel, the opposite
// holds: an ungrabbed corner tap is delivered to both this recognizer and the
// invisible interface below, which quietly navigates somewhere else and is
// waiting there when the panel is handed back.
//
// The grab is released by the kernel when the descriptor closes, so a crashed
// or killed Guardian cannot leave the touchscreen owned by a dead process.
func (s *EvdevSource) Grab(exclusive bool) error {
	argument := uintptr(0)
	if exclusive {
		argument = 1
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, s.file.Fd(), eviocgrab, argument); errno != 0 {
		return errno
	}
	return nil
}

// inputGrabber is satisfied by EvdevSource. It is deliberately not part of
// InputSource: a recorded event stream has nothing to grab, and the recognizer
// tests must not have to pretend otherwise.
type inputGrabber interface {
	Grab(exclusive bool) error
}

var _ inputGrabber = (*EvdevSource)(nil)

// Next blocks in the read; Close is what unblocks it on shutdown.
func (s *EvdevSource) Next(ctx context.Context) (InputEvent, error) {
	if err := ctx.Err(); err != nil {
		return InputEvent{}, err
	}
	if _, err := io.ReadFull(s.file, s.buf); err != nil {
		return InputEvent{}, err
	}
	return decodeInputEvent(s.buf)
}

func (s *EvdevSource) Close() error { return s.file.Close() }

var _ InputSource = (*EvdevSource)(nil)

// Zone is an inclusive rectangle in framebuffer coordinates.
type Zone struct {
	MinX, MinY, MaxX, MaxY int
}

func (z Zone) contains(x, y int) bool {
	return x >= z.MinX && x <= z.MaxX && y >= z.MinY && y <= z.MaxY
}

// CornerZones builds the four corner hot zones: squares whose side is 15% of
// the screen's short edge, so the same rule holds on whatever panel the probe
// reports rather than on a hard-coded PW4 geometry.
func CornerZones(screen ScreenCapabilities) []Zone {
	short := screen.Width
	if screen.Height < short {
		short = screen.Height
	}
	side := short * 15 / 100
	if side < 1 {
		side = 1
	}
	return []Zone{
		{MinX: 0, MinY: 0, MaxX: side - 1, MaxY: side - 1},
		{MinX: screen.Width - side, MinY: 0, MaxX: screen.Width - 1, MaxY: side - 1},
		{MinX: 0, MinY: screen.Height - side, MaxX: side - 1, MaxY: screen.Height - 1},
		{MinX: screen.Width - side, MinY: screen.Height - side, MaxX: screen.Width - 1, MaxY: screen.Height - 1},
	}
}

// GestureRecognizer turns a multitouch stream into "one finger tapped one
// corner three times within the tap window". Everything else — a tap outside
// every zone, taps split across corners, slow taps, a second finger, a long
// press — cancels or never counts, so ordinary reading gestures can never
// leave exclusive mode.
type GestureRecognizer struct {
	Zones []Zone
	// Window bounds the whole three-tap sequence (first to third touch-down).
	Window time.Duration

	slot     int32
	contacts map[int32]touchContact
	desynced bool
	pressed  bool
	x, y     int

	zone        int // zone of the current tap sequence, -1 = none
	taps        int
	windowStart time.Time
	prevFingers int
}

// touchContact keeps Type-B coordinates with the slot that supplied them.
// Linux deliberately leaves coordinates from other slots in the stream, so a
// single global x/y pair can turn a newly touched slot into a false corner
// hold.
type touchContact struct {
	x, y       int
	hasX, hasY bool
}

func NewGestureRecognizer(zones []Zone, window time.Duration) *GestureRecognizer {
	if window <= 0 {
		window = time.Second
	}
	return &GestureRecognizer{Zones: zones, Window: window, contacts: map[int32]touchContact{}, zone: -1}
}

// fingers prefers the multitouch slots and falls back to BTN_TOUCH, so a driver
// that reports only the single-touch protocol still works while a second
// tracked contact still cancels.
func (g *GestureRecognizer) fingers() int {
	if len(g.contacts) > 0 {
		return len(g.contacts)
	}
	if g.pressed {
		return 1
	}
	return 0
}

// Feed consumes one event and reports whether the gesture has just completed.
// Only SYN_REPORT evaluates, because that is where the kernel guarantees the
// contact state is consistent.
func (g *GestureRecognizer) Feed(event InputEvent) bool {
	// SYN_DROPPED means the kernel discarded part of the stream.  Continuing
	// with the old contact state could fabricate taps, so discard frames
	// through the next synchronization point.
	if event.Type == evSyn && event.Code == synDropped {
		g.contacts = map[int32]touchContact{}
		g.pressed = false
		g.desynced = true
		g.prevFingers = 0
		g.resetSequence()
		return false
	}
	if g.desynced {
		if event.Type == evSyn && event.Code == synReport {
			g.desynced = false
		}
		return false
	}
	switch event.Type {
	case evAbs:
		switch event.Code {
		case absMTSlot:
			g.slot = event.Value
		case absMTTrackingID:
			if event.Value < 0 {
				delete(g.contacts, g.slot)
			} else {
				// A tracking ID begins a new contact; old coordinates from this
				// slot must not survive the previous finger.
				g.contacts[g.slot] = touchContact{}
			}
		case absMTPositionX:
			contact, ok := g.contacts[g.slot]
			if ok {
				contact.x, contact.hasX = int(event.Value), true
				g.contacts[g.slot] = contact
			}
		case absMTPositionY:
			contact, ok := g.contacts[g.slot]
			if ok {
				contact.y, contact.hasY = int(event.Value), true
				g.contacts[g.slot] = contact
			}
		case absX:
			g.x = int(event.Value)
		case absY:
			g.y = int(event.Value)
		}
	case evKey:
		if event.Code == btnTouch {
			g.pressed = event.Value != 0
		}
	case evSyn:
		if event.Code == synReport {
			return g.evaluate(event.At)
		}
	}
	return false
}

// Tick keeps no clock-driven state: a three-tap sequence either completes on
// the third touch-down event or is re-based by the next tap, so there is no
// deadline to enforce between events.
func (g *GestureRecognizer) Tick(time.Time) bool {
	return false
}

func (g *GestureRecognizer) evaluate(at time.Time) bool {
	fingers := g.fingers()
	defer func() { g.prevFingers = fingers }()
	if fingers > 1 {
		// A second finger invalidates any in-flight sequence.
		g.resetSequence()
		return false
	}
	if fingers != 1 || g.prevFingers != 0 {
		return false
	}
	// A fresh touch-down: resolve its coordinates.
	x, y := g.x, g.y
	if len(g.contacts) == 1 {
		for _, contact := range g.contacts {
			if !contact.hasX || !contact.hasY {
				g.resetSequence()
				return false
			}
			x, y = contact.x, contact.y
		}
	}
	zone := g.zoneAt(x, y)
	if zone < 0 {
		// A touch outside every zone breaks the sequence.
		g.resetSequence()
		return false
	}
	if zone != g.zone || g.taps == 0 || at.Sub(g.windowStart) > g.Window {
		g.zone = zone
		g.taps = 1
		g.windowStart = at
		return false
	}
	g.taps++
	if g.taps < 3 {
		return false
	}
	g.resetSequence()
	return true
}

func (g *GestureRecognizer) resetSequence() {
	g.zone = -1
	g.taps = 0
	if len(g.contacts) == 0 {
		g.x, g.y = 0, 0
	}
}

func (g *GestureRecognizer) zoneAt(x, y int) int {
	for index, zone := range g.Zones {
		if zone.contains(x, y) {
			return index
		}
	}
	return -1
}

// GestureWatcher runs the recognizer against a live device.
type GestureWatcher struct {
	Source     InputSource
	Recognizer *GestureRecognizer
	Poll       time.Duration
	Retry      time.Duration
	Now        func() time.Time
	OnTrigger  func(context.Context)
	// Exclusive reports whether the panel is currently covered by this
	// service. While it is, the touch stream is taken exclusively so taps do
	// not also reach the native interface underneath. Nil disables the step.
	Exclusive func() bool
	// Changed wakes the watcher the moment exclusivity flips, so the grab is
	// released as soon as the panel is handed back rather than up to one poll
	// interval later — a reader whose screen has just come back should not
	// have their first tap swallowed. Nil falls back to the poll tick.
	Changed <-chan struct{}

	grabbed bool
}

// reconcileGrab drives the grab towards what Exclusive currently reports. It
// runs on every exclusivity change and on the poll tick, because exclusive mode
// is entered and left while the same descriptor stays open, and because the
// change signal can be missed while the descriptor is being reopened. A failed
// ioctl leaves the recorded state alone so the next pass retries.
func (w *GestureWatcher) reconcileGrab() {
	if w.Exclusive == nil {
		return
	}
	grabber, ok := w.Source.(inputGrabber)
	if !ok {
		return
	}
	wanted := w.Exclusive()
	if wanted == w.grabbed {
		return
	}
	if grabber.Grab(wanted) != nil {
		return
	}
	w.grabbed = wanted
}

// releaseGrab is best effort on the way out. The descriptor close that follows
// would release it anyway; doing it explicitly keeps the window in which a
// still-open device is owned by a watcher that has stopped reading as short as
// possible.
func (w *GestureWatcher) releaseGrab() {
	if !w.grabbed {
		return
	}
	if grabber, ok := w.Source.(inputGrabber); ok {
		_ = grabber.Grab(false)
	}
	w.grabbed = false
}

func (w *GestureWatcher) retry() time.Duration {
	if w.Retry <= 0 {
		return 250 * time.Millisecond
	}
	return w.Retry
}

func (w *GestureWatcher) poll() time.Duration {
	if w.Poll <= 0 {
		return 100 * time.Millisecond
	}
	return w.Poll
}

func (w *GestureWatcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *GestureWatcher) Watch(ctx context.Context) error {
	events := make(chan InputEvent)
	failures := make(chan error, 1)
	go func() {
		defer close(events)
		consecutiveFailures := 0
		for {
			event, err := w.Source.Next(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				// Device reads can fail while the input subsystem is settling. A
				// few errors are retried on the same descriptor, but a persistently
				// dead descriptor is handed back to startGestureWatcher so it can
				// reopen a re-enumerated touch node.
				consecutiveFailures++
				if consecutiveFailures >= 3 {
					select {
					case failures <- err:
					default:
					}
					return
				}
				timer := time.NewTimer(w.retry())
				select {
				case <-ctx.Done():
					if !timer.Stop() {
						<-timer.C
					}
					return
				case <-timer.C:
					continue
				}
			}
			consecutiveFailures = 0
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	ticker := time.NewTicker(w.poll())
	defer ticker.Stop()
	defer w.releaseGrab()
	w.reconcileGrab()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case event, ok := <-events:
			if !ok {
				return nil
			}
			if w.Recognizer.Feed(event) {
				w.trigger(ctx)
			}
		case <-w.Changed:
			w.reconcileGrab()
		case <-ticker.C:
			w.reconcileGrab()
			if w.Recognizer.Tick(w.now()) {
				w.trigger(ctx)
			}
		}
	}
}

func (w *GestureWatcher) trigger(ctx context.Context) {
	if w.OnTrigger != nil {
		w.OnTrigger(ctx)
	}
}

// The reopen loop below runs only while the touch node cannot be opened, so
// its cost is paid exactly when the physical exit is already degraded. The
// first retry stays fast because the usual cause is the input stack still
// settling a moment after entry; the cap keeps a permanently misconfigured
// EINKRELAY_INPUT_DEVICE from spinning the CPU and the battery for the whole
// session. Five seconds is the longest a corner press can go unnoticed once
// the node comes back, which is well inside the time it takes a user to walk
// over to the device and try again.
const (
	gestureRetryInitial = 250 * time.Millisecond
	gestureRetryMax     = 5 * time.Second
	// gestureLogInterval throttles the "still unavailable" line. Unthrottled,
	// a missing touch node wrote four lines a second — around 350,000 a day —
	// into a log that lives on the Kindle's small root partition. The first
	// failure and the recovery are always reported; the repetitions in between
	// are summarised.
	gestureLogInterval = 5 * time.Minute
)

// gestureRetryBackoff doubles up to the cap. It is a plain function so the
// schedule is visible in one place and testable without a device.
func gestureRetryBackoff(attempt int) time.Duration {
	delay := gestureRetryInitial
	for i := 0; i < attempt && delay < gestureRetryMax; i++ {
		delay *= 2
	}
	if delay > gestureRetryMax {
		delay = gestureRetryMax
	}
	return delay
}

// gestureLogThrottle collapses a repeating failure into one line plus periodic
// summaries, and reports the recovery exactly once.
type gestureLogThrottle struct {
	log        func(string)
	now        func() time.Time
	reason     string
	suppressed int
	lastLogged time.Time
}

func (t *gestureLogThrottle) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func (t *gestureLogThrottle) failure(reason string) {
	at := t.clock()
	if reason != t.reason {
		t.reason = reason
		t.suppressed = 0
		t.lastLogged = at
		t.log(reason + "; retrying corner exit gesture")
		return
	}
	t.suppressed++
	if at.Sub(t.lastLogged) < gestureLogInterval {
		return
	}
	t.log(fmt.Sprintf("%s; still retrying corner exit gesture (%d further attempts)", reason, t.suppressed))
	t.suppressed = 0
	t.lastLogged = at
}

func (t *gestureLogThrottle) recovered() {
	if t.reason == "" {
		return
	}
	t.log("the corner exit gesture is watching the touch device again")
	t.reason = ""
	t.suppressed = 0
}

// startGestureWatcher wires the corner long-press to the exit coordinator. A
// gesture that cannot be started is logged and skipped on purpose: an
// unreadable touch node or framebuffer must not take the supervisor down with
// it, because supervision and the REST exit path still work without it.
func startGestureWatcher(ctx context.Context, cfg Config, coordinator *ExitCoordinator, exclusive func() bool, changed <-chan struct{}, log func(string)) func() {
	watchCtx, cancel := context.WithCancel(ctx)
	var sourceMu sync.Mutex
	var source InputSource
	go func() {
		throttle := &gestureLogThrottle{log: log}
		attempt := 0
		for {
			// Both the framebuffer probe and opening the node may briefly fail
			// while the Kindle input stack is starting.  Retry them too; an
			// initial failure must not permanently remove the physical exit.
			screen, err := (&SysfsScreenProbe{}).Probe(watchCtx)
			if err != nil {
				throttle.failure("the screen geometry is unreadable")
				if !waitGestureRetry(watchCtx, gestureRetryBackoff(attempt)) {
					return
				}
				attempt++
				continue
			}
			opened, err := OpenEvdev(cfg.InputDevice)
			if err != nil {
				throttle.failure("the touch device is unavailable")
				if !waitGestureRetry(watchCtx, gestureRetryBackoff(attempt)) {
					return
				}
				attempt++
				continue
			}
			throttle.recovered()
			attempt = 0
			sourceMu.Lock()
			source = opened
			sourceMu.Unlock()
			watcher := &GestureWatcher{
				Source: opened, Recognizer: NewGestureRecognizer(CornerZones(screen), cfg.GestureTapWindow),
				Exclusive: exclusive, Changed: changed,
				OnTrigger: func(triggerCtx context.Context) {
					// The same coordinator the control socket calls, so the corner and
					// POST /v1/system/exit share one recovery implementation.
					if coordinator.Exit(triggerCtx, exitReasonGesture) != nil {
						log("the corner exit reported a failure")
					}
				},
			}
			_ = watcher.Watch(watchCtx)
			_ = opened.Close()
			sourceMu.Lock()
			if source == opened {
				source = nil
			}
			sourceMu.Unlock()
			if watchCtx.Err() != nil {
				return
			}
			// A watcher that had been reading and then lost the descriptor is
			// a fresh failure, not a continuation of an earlier one, so the
			// backoff starts over rather than inheriting a long delay.
			if !waitGestureRetry(watchCtx, gestureRetryInitial) {
				return
			}
		}
	}()
	return func() {
		cancel()
		sourceMu.Lock()
		if source != nil {
			_ = source.Close()
		}
		sourceMu.Unlock()
	}
}

func waitGestureRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
