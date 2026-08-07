package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDecodeInputEventPW4Recording(t *testing.T) {
	// PW4 is ARMv7: struct input_event has two 32-bit timeval fields, then
	// type/code/value, for a 16-byte little-endian event frame.
	raw := make([]byte, 16)
	binary.LittleEndian.PutUint32(raw[0:4], 123)
	binary.LittleEndian.PutUint32(raw[4:8], 456000)
	binary.LittleEndian.PutUint16(raw[8:10], evAbs)
	binary.LittleEndian.PutUint16(raw[10:12], absMTPositionX)
	binary.LittleEndian.PutUint32(raw[12:16], 42)
	event, err := decodeInputEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != evAbs || event.Code != absMTPositionX || event.Value != 42 || !event.At.Equal(time.Unix(123, 456000000).UTC()) {
		t.Fatalf("decoded event = %#v", event)
	}
}

func gestureEvent(at time.Time, typ, code uint16, value int32) InputEvent {
	return InputEvent{Type: typ, Code: code, Value: value, At: at}
}

func report(at time.Time) InputEvent { return gestureEvent(at, evSyn, synReport, 0) }

func TestOpenEvdevRejectsUnsafeDeviceNamesAndLinks(t *testing.T) {
	for _, path := range []string{"/dev/input/event0", "/dev/input/event1", "/dev/input/event3"} {
		if _, err := OpenEvdev(path); !errors.Is(err, ErrForbiddenInputDevice) {
			t.Errorf("OpenEvdev(%q) error = %v, want forbidden", path, err)
		}
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "event0")
	if err := os.WriteFile(target, []byte("not an input device"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "event2")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenEvdev(link); !errors.Is(err, ErrForbiddenInputDevice) {
		t.Fatalf("OpenEvdev symlink error = %v, want forbidden", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	regularEvent2 := filepath.Join(dir, "event2")
	if err := os.WriteFile(regularEvent2, []byte("not a character device"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenEvdev(regularEvent2); !errors.Is(err, ErrForbiddenInputDevice) {
		t.Fatalf("OpenEvdev regular file error = %v, want forbidden", err)
	}
}

// tap is one quick touch-down + touch-up cycle at (x, y) on slot 0.
func tap(start time.Time, id, x, y int32) []InputEvent {
	return []InputEvent{
		gestureEvent(start, evAbs, absMTSlot, 0),
		gestureEvent(start, evAbs, absMTTrackingID, id),
		gestureEvent(start, evAbs, absMTPositionX, x),
		gestureEvent(start, evAbs, absMTPositionY, y),
		report(start),
		gestureEvent(start.Add(60*time.Millisecond), evAbs, absMTTrackingID, -1),
		report(start.Add(60 * time.Millisecond)),
	}
}

// The exit gesture is three quick taps inside one corner within one second,
// on any of the four corners. A long press no longer means anything.
func TestGestureRecognizerTripleTapOnEveryCorner(t *testing.T) {
	screen := ScreenCapabilities{Width: 1000, Height: 1400}
	side := int32(150) // 15% of the short edge
	corners := [][2]int32{
		{10, 10},               // top-left
		{1000 - 10, 10},        // top-right
		{10, 1400 - 10},        // bottom-left
		{1000 - 10, 1400 - 10}, // bottom-right
	}
	_ = side
	for _, point := range corners {
		start := time.Unix(100, 0)
		g := NewGestureRecognizer(CornerZones(screen), time.Second)
		fired := false
		for i := range 3 {
			for _, e := range tap(start.Add(time.Duration(i)*250*time.Millisecond), int32(i+1), point[0], point[1]) {
				if g.Feed(e) {
					if i < 2 {
						t.Fatalf("corner %#v fired on tap %d", point, i+1)
					}
					fired = true
				}
			}
		}
		if !fired {
			t.Fatalf("corner %#v did not trigger on the third tap", point)
		}
		// A fourth tap starts a fresh sequence; it must not fire by itself.
		for _, e := range tap(start.Add(800*time.Millisecond), 4, point[0], point[1]) {
			if g.Feed(e) {
				t.Fatalf("corner %#v double-fired", point)
			}
		}
	}
}

func TestGestureRecognizerRejectsNonTripleTapPatterns(t *testing.T) {
	screen := ScreenCapabilities{Width: 1000, Height: 1400}
	cases := []struct {
		name   string
		events []InputEvent
	}{
		{"two taps only", append(append([]InputEvent{}, tap(time.Unix(100, 0), 1, 10, 10)...), tap(time.Unix(100, 0).Add(250*time.Millisecond), 2, 10, 10)...)},
		{"taps in different corners", append(append(append([]InputEvent{}, tap(time.Unix(100, 0), 1, 10, 10)...), tap(time.Unix(100, 0).Add(250*time.Millisecond), 2, 990, 10)...), tap(time.Unix(100, 0).Add(500*time.Millisecond), 3, 10, 10)...)},
		{"taps outside the zones", append(append(append([]InputEvent{}, tap(time.Unix(100, 0), 1, 400, 400)...), tap(time.Unix(100, 0).Add(250*time.Millisecond), 2, 400, 400)...), tap(time.Unix(100, 0).Add(500*time.Millisecond), 3, 400, 400)...)},
		{"slow taps exceed the window", append(append(append([]InputEvent{}, tap(time.Unix(100, 0), 1, 10, 10)...), tap(time.Unix(100, 0).Add(700*time.Millisecond), 2, 10, 10)...), tap(time.Unix(100, 0).Add(1400*time.Millisecond), 3, 10, 10)...)},
		{"long press is not a trigger", func() []InputEvent {
			start := time.Unix(100, 0)
			return []InputEvent{
				gestureEvent(start, evAbs, absMTSlot, 0),
				gestureEvent(start, evAbs, absMTTrackingID, 1),
				gestureEvent(start, evAbs, absMTPositionX, 10),
				gestureEvent(start, evAbs, absMTPositionY, 10), report(start),
				report(start.Add(2 * time.Second)),
				report(start.Add(4 * time.Second)),
				gestureEvent(start.Add(4*time.Second), evAbs, absMTTrackingID, -1),
				report(start.Add(4 * time.Second)),
			}
		}()},
		{"multi touch cancels", func() []InputEvent {
			start := time.Unix(100, 0)
			events := tap(start, 1, 10, 10)
			events = append(events,
				gestureEvent(start.Add(200*time.Millisecond), evAbs, absMTSlot, 0),
				gestureEvent(start.Add(200*time.Millisecond), evAbs, absMTTrackingID, 2),
				gestureEvent(start.Add(200*time.Millisecond), evAbs, absMTPositionX, 10),
				gestureEvent(start.Add(200*time.Millisecond), evAbs, absMTPositionY, 10),
				gestureEvent(start.Add(200*time.Millisecond), evAbs, absMTSlot, 1),
				gestureEvent(start.Add(200*time.Millisecond), evAbs, absMTTrackingID, 3),
				report(start.Add(200*time.Millisecond)))
			events = append(events, tap(start.Add(400*time.Millisecond), 4, 10, 10)...)
			return events
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGestureRecognizer(CornerZones(screen), time.Second)
			for _, e := range tc.events {
				if g.Feed(e) {
					t.Fatalf("%s triggered", tc.name)
				}
			}
		})
	}
}

func TestGestureRecognizerDoesNotReuseOtherSlotCoordinates(t *testing.T) {
	start := time.Unix(200, 0)
	g := NewGestureRecognizer(CornerZones(ScreenCapabilities{Width: 1000, Height: 1400}), time.Second)
	// Slot 0 was in a hot corner, but was lifted. Slot 1 must not inherit its
	// coordinates just because the driver has not reported slot 1 coordinates.
	for _, e := range []InputEvent{
		gestureEvent(start, evAbs, absMTSlot, 0), gestureEvent(start, evAbs, absMTTrackingID, 10),
		gestureEvent(start, evAbs, absMTPositionX, 10), gestureEvent(start, evAbs, absMTPositionY, 10), report(start),
		gestureEvent(start.Add(time.Millisecond), evAbs, absMTTrackingID, -1),
		gestureEvent(start.Add(time.Millisecond), evAbs, absMTSlot, 1), gestureEvent(start.Add(time.Millisecond), evAbs, absMTTrackingID, 11), report(start.Add(time.Millisecond)),
	} {
		g.Feed(e)
	}
	if g.Tick(start.Add(2 * time.Second)) {
		t.Fatal("new slot inherited released slot coordinates")
	}
}

func TestGestureRecognizerDiscardsStateAfterSynDropped(t *testing.T) {
	start := time.Unix(250, 0)
	g := NewGestureRecognizer(CornerZones(ScreenCapabilities{Width: 1000, Height: 1400}), time.Second)
	for _, e := range []InputEvent{
		gestureEvent(start, evAbs, absMTSlot, 0), gestureEvent(start, evAbs, absMTTrackingID, 10),
		gestureEvent(start, evAbs, absMTPositionX, 10), gestureEvent(start, evAbs, absMTPositionY, 10), report(start),
		gestureEvent(start.Add(time.Millisecond), evSyn, synDropped, 0), report(start.Add(2 * time.Millisecond)),
	} {
		g.Feed(e)
	}
	if g.Tick(start.Add(2 * time.Second)) {
		t.Fatal("dropped stream retained a held corner contact")
	}
}

func TestGestureRecognizerSynDroppedCancelsSingleTouchFallback(t *testing.T) {
	start := time.Unix(275, 0)
	g := NewGestureRecognizer(CornerZones(ScreenCapabilities{Width: 1000, Height: 1400}), time.Second)
	for _, e := range []InputEvent{
		gestureEvent(start, evKey, btnTouch, 1),
		gestureEvent(start, evAbs, absX, 10), gestureEvent(start, evAbs, absY, 10), report(start),
		gestureEvent(start.Add(time.Millisecond), evSyn, synDropped, 0),
		report(start.Add(2 * time.Millisecond)),
		report(start.Add(3 * time.Millisecond)),
	} {
		g.Feed(e)
	}
	if g.Tick(start.Add(2 * time.Second)) {
		t.Fatal("dropped single-touch stream retained a corner press")
	}
}

type retryInput struct {
	mu        sync.Mutex
	remaining int
	event     InputEvent
	delivered bool
}

func (s *retryInput) Next(ctx context.Context) (InputEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remaining > 0 {
		s.remaining--
		return InputEvent{}, errors.New("temporary read failure")
	}
	if s.delivered {
		s.mu.Unlock()
		defer s.mu.Lock()
		<-ctx.Done()
		return InputEvent{}, ctx.Err()
	}
	s.delivered = true
	select {
	case <-ctx.Done():
		return InputEvent{}, ctx.Err()
	default:
	}
	return s.event, nil
}
func (*retryInput) Close() error { return nil }

func TestGestureWatcherRetriesTransientReadError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Unix(300, 0)
	source := &retryInput{remaining: 1, event: report(start)}
	g := NewGestureRecognizer([]Zone{{MinX: 0, MinY: 0, MaxX: 20, MaxY: 20}}, time.Second)
	// Preserve a coherent tap sequence across the transient reader error: the
	// recovered SYN_REPORT must still count as the third tap and fire.
	g.pressed, g.x, g.y = true, 10, 10
	g.zone, g.taps, g.windowStart = 0, 2, start.Add(-500*time.Millisecond)
	triggered := make(chan struct{}, 1)
	w := &GestureWatcher{Source: source, Recognizer: g, Poll: time.Millisecond, Retry: time.Millisecond, OnTrigger: func(context.Context) { triggered <- struct{}{} }}
	done := make(chan error, 1)
	go func() { done <- w.Watch(ctx) }()
	select {
	case <-triggered:
	case <-time.After(time.Second):
		t.Fatal("watcher did not recover after a transient read failure")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Watch error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("watcher did not stop")
	}
}

func TestGestureWatcherReturnsAfterPersistentReadErrorsForReopen(t *testing.T) {
	source := &retryInput{remaining: 3}
	w := &GestureWatcher{Source: source, Recognizer: NewGestureRecognizer(nil, time.Second), Retry: time.Millisecond}
	if err := w.Watch(context.Background()); err == nil {
		t.Fatal("persistent read failure did not return for outer reopen")
	}
}

// TestGestureRetryBackoffIsBoundedAndStartsFast pins both ends of the reopen
// schedule: the first retry has to stay fast, because the usual cause is the
// input stack settling a moment after entry, and the schedule has to stop
// doubling, because an EINKRELAY_INPUT_DEVICE that will never open must not
// spin the CPU for the whole session.
func TestGestureRetryBackoffIsBoundedAndStartsFast(t *testing.T) {
	if got := gestureRetryBackoff(0); got != gestureRetryInitial {
		t.Fatalf("first retry = %v, want %v", got, gestureRetryInitial)
	}
	previous := time.Duration(0)
	for attempt := 0; attempt < 20; attempt++ {
		delay := gestureRetryBackoff(attempt)
		if delay < gestureRetryInitial || delay > gestureRetryMax {
			t.Fatalf("attempt %d produced %v, outside [%v, %v]", attempt, delay, gestureRetryInitial, gestureRetryMax)
		}
		if delay < previous {
			t.Fatalf("attempt %d went backwards: %v after %v", attempt, delay, previous)
		}
		previous = delay
	}
	if previous != gestureRetryMax {
		t.Fatalf("the schedule never reached its cap: %v", previous)
	}
}

// TestGestureLogThrottleSummarisesRepeatsAndReportsRecovery is the guard on the
// log-volume defect: an unavailable touch node used to write one line per
// retry, which on the Kindle's small root partition is hundreds of thousands of
// lines a day. The first failure and the recovery must still be reported.
func TestGestureLogThrottleSummarisesRepeatsAndReportsRecovery(t *testing.T) {
	var lines []string
	now := time.Unix(0, 0).UTC()
	throttle := &gestureLogThrottle{
		log: func(message string) { lines = append(lines, message) },
		now: func() time.Time { return now },
	}

	throttle.failure("the touch device is unavailable")
	if len(lines) != 1 || !strings.Contains(lines[0], "the touch device is unavailable") {
		t.Fatalf("the first failure was not reported: %v", lines)
	}
	for i := 0; i < 2000; i++ {
		now = now.Add(100 * time.Millisecond)
		throttle.failure("the touch device is unavailable")
	}
	// 2000 retries at 100ms is a little over three minutes, still inside the
	// interval: exactly one line so far.
	if len(lines) != 1 {
		t.Fatalf("repeats were not throttled: %v", lines)
	}
	now = now.Add(gestureLogInterval)
	throttle.failure("the touch device is unavailable")
	if len(lines) != 2 || !strings.Contains(lines[1], "still retrying") {
		t.Fatalf("no periodic summary was emitted: %v", lines)
	}

	// A different cause is new information and is always reported.
	throttle.failure("the screen geometry is unreadable")
	if len(lines) != 3 || !strings.Contains(lines[2], "the screen geometry is unreadable") {
		t.Fatalf("a changed cause was swallowed: %v", lines)
	}

	throttle.recovered()
	if len(lines) != 4 || !strings.Contains(lines[3], "again") {
		t.Fatalf("the recovery was not reported: %v", lines)
	}
	// Recovery is reported once, not on every healthy iteration.
	throttle.recovered()
	if len(lines) != 4 {
		t.Fatalf("recovery was reported more than once: %v", lines)
	}
}

// grabbableInput is an InputSource that also implements inputGrabber, which is
// what the real EvdevSource does. It records every transition so a test can
// assert that the grab follows exclusive mode rather than being taken once and
// forgotten.
type grabbableInput struct {
	mu       sync.Mutex
	events   <-chan InputEvent
	calls    []bool
	failNext bool
	grabbed  bool
}

func (g *grabbableInput) Next(ctx context.Context) (InputEvent, error) {
	select {
	case event, ok := <-g.events:
		if !ok {
			return InputEvent{}, io.EOF
		}
		return event, nil
	case <-ctx.Done():
		return InputEvent{}, ctx.Err()
	}
}

func (g *grabbableInput) Close() error { return nil }

func (g *grabbableInput) Grab(exclusive bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failNext {
		g.failNext = false
		return errors.New("ioctl refused")
	}
	g.calls = append(g.calls, exclusive)
	g.grabbed = exclusive
	return nil
}

func (g *grabbableInput) transitions() []bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]bool(nil), g.calls...)
}

func (g *grabbableInput) held() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.grabbed
}

// TestGestureWatcherGrabsOnlyWhileExclusive is the invariant behind taking the
// touch stream. While the native interface is visible it must keep receiving
// taps; while it is covered by this service it must not, or a corner triple-tap
// also navigates the invisible interface underneath and the panel is handed
// back somewhere the reader never asked to be.
func TestGestureWatcherGrabsOnlyWhileExclusive(t *testing.T) {
	source := &grabbableInput{events: make(chan InputEvent)}
	exclusive := false
	watcher := &GestureWatcher{
		Source:     source,
		Recognizer: NewGestureRecognizer(CornerZones(ScreenCapabilities{Width: 100, Height: 100}), time.Second),
		Exclusive:  func() bool { return exclusive },
	}

	watcher.reconcileGrab()
	if source.held() || len(source.transitions()) != 0 {
		t.Fatalf("grabbed while the native interface was visible: %v", source.transitions())
	}

	exclusive = true
	watcher.reconcileGrab()
	if !source.held() {
		t.Fatal("entering exclusive mode did not take the touch stream")
	}
	// Reconciling again must not re-issue the ioctl: it runs on every poll tick.
	watcher.reconcileGrab()
	if got := source.transitions(); len(got) != 1 || !got[0] {
		t.Fatalf("transitions = %v, want exactly one grab", got)
	}

	exclusive = false
	watcher.reconcileGrab()
	if source.held() {
		t.Fatal("leaving exclusive mode did not release the touch stream")
	}
	if got := source.transitions(); len(got) != 2 || got[1] {
		t.Fatalf("transitions = %v, want a grab then a release", got)
	}
}

// TestGestureWatcherRetriesAFailedGrab keeps a transient ioctl failure from
// permanently leaving the interface underneath receiving taps.
func TestGestureWatcherRetriesAFailedGrab(t *testing.T) {
	source := &grabbableInput{events: make(chan InputEvent), failNext: true}
	watcher := &GestureWatcher{
		Source:     source,
		Recognizer: NewGestureRecognizer(CornerZones(ScreenCapabilities{Width: 100, Height: 100}), time.Second),
		Exclusive:  func() bool { return true },
	}

	watcher.reconcileGrab()
	if source.held() {
		t.Fatal("a failed ioctl was recorded as a successful grab")
	}
	watcher.reconcileGrab()
	if !source.held() {
		t.Fatal("the grab was not retried after a failure")
	}
}

// TestGestureWatcherWithoutAnExclusivePredicateNeverGrabs covers the recognizer
// tests and any recorded-stream source: nothing to grab, nothing attempted.
func TestGestureWatcherWithoutAnExclusivePredicateNeverGrabs(t *testing.T) {
	source := &grabbableInput{events: make(chan InputEvent)}
	watcher := &GestureWatcher{
		Source:     source,
		Recognizer: NewGestureRecognizer(CornerZones(ScreenCapabilities{Width: 100, Height: 100}), time.Second),
	}
	watcher.reconcileGrab()
	watcher.releaseGrab()
	if len(source.transitions()) != 0 {
		t.Fatalf("grab was attempted without an exclusive predicate: %v", source.transitions())
	}
}

// TestGestureWatcherReleasesTheGrabWhenItStops covers the shutdown edge: the
// descriptor close would release it anyway, but the window in which a watcher
// that has stopped reading still owns the device should be as short as possible.
func TestGestureWatcherReleasesTheGrabWhenItStops(t *testing.T) {
	events := make(chan InputEvent)
	source := &grabbableInput{events: events}
	watcher := &GestureWatcher{
		Source:     source,
		Recognizer: NewGestureRecognizer(CornerZones(ScreenCapabilities{Width: 100, Height: 100}), time.Second),
		Poll:       time.Millisecond,
		Exclusive:  func() bool { return true },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = watcher.Watch(ctx) }()

	deadline := time.After(2 * time.Second)
	for !source.held() {
		select {
		case <-deadline:
			t.Fatal("the watcher never took the touch stream")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
	if source.held() {
		t.Fatal("the watcher stopped while still holding the touch stream")
	}
}

// TestGestureWatcherReleasesTheGrabOnNotificationNotOnTheTick pins the reason
// the lifecycle signals instead of the watcher polling: the moment the panel is
// handed back, taps have to reach the interface the reader can now see. Waiting
// for a poll tick would swallow the first tap after every exit.
func TestGestureWatcherReleasesTheGrabOnNotificationNotOnTheTick(t *testing.T) {
	source := &grabbableInput{events: make(chan InputEvent)}
	exclusive := true
	changed := make(chan struct{}, 1)
	watcher := &GestureWatcher{
		Source:     source,
		Recognizer: NewGestureRecognizer(CornerZones(ScreenCapabilities{Width: 100, Height: 100}), time.Second),
		// A poll interval far longer than the test: if the release depended on
		// the tick, this would time out.
		Poll:      time.Hour,
		Exclusive: func() bool { return exclusive },
		Changed:   changed,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = watcher.Watch(ctx) }()

	deadline := time.After(2 * time.Second)
	for !source.held() {
		select {
		case <-deadline:
			t.Fatal("the watcher never took the touch stream")
		case <-time.After(time.Millisecond):
		}
	}
	exclusive = false
	changed <- struct{}{}
	deadline = time.After(2 * time.Second)
	for source.held() {
		select {
		case <-deadline:
			t.Fatal("the grab was not released on the change notification")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
}
