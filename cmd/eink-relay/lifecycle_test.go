package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeNativeUI records the property writes exclusive mode is made of. It is
// deliberately a recorder rather than a state machine: every operation in the
// port states a desired end state instead of a transition, so what a test needs
// to assert is which ones were issued, in what order, and that a failure in one
// never silently swallows another.
type fakeNativeUI struct {
	mu         sync.Mutex
	calls      []string
	holdErr    error
	freeErr    error
	restoreErr error
	held       bool
}

func (n *fakeNativeUI) record(name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, name)
}

func (n *fakeNativeUI) HoldPanel(context.Context) error {
	n.record("hold")
	if n.holdErr != nil {
		return n.holdErr
	}
	n.mu.Lock()
	n.held = true
	n.mu.Unlock()
	return nil
}

func (n *fakeNativeUI) ReleasePanel(context.Context) error {
	n.record("release")
	if n.freeErr != nil {
		return n.freeErr
	}
	n.mu.Lock()
	n.held = false
	n.mu.Unlock()
	return nil
}

// fakeSnapshot records the panel capture/restore that replaced the attempt to
// make the native interface redraw itself.
type fakeSnapshot struct {
	native     *fakeNativeUI
	captureErr error
	restoreErr error
	captured   bool
}

func (f *fakeSnapshot) Capture(context.Context) error {
	f.native.record("capture")
	if f.captureErr != nil {
		return f.captureErr
	}
	f.captured = true
	return nil
}

func (f *fakeSnapshot) Restore(context.Context) error {
	f.native.record("restore")
	return f.restoreErr
}

func (f *fakeSnapshot) Owed() bool { return f.captured }

func (f *fakeSnapshot) Discard() error {
	f.native.record("discard")
	f.captured = false
	return nil
}

func (n *fakeNativeUI) sequence() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.calls...)
}

func (n *fakeNativeUI) holding() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.held
}

func equalSequence(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func testLifecycle(native *fakeNativeUI) *Lifecycle {
	return &Lifecycle{Native: native, Snapshot: &fakeSnapshot{native: native}, Timeout: 25 * time.Millisecond}
}

// TestEnterUnlocksTheTouchController pins the PW4 finding: the goodix touch
// controller can be left in a locked sleep with the event stream at zero, so
// entry writes `unlock` to the node.
func TestEnterUnlocksTheTouchController(t *testing.T) {
	node := filepath.Join(t.TempDir(), "touch")
	if err := os.WriteFile(node, []byte("Touch is locked\n"), 0644); err != nil {
		t.Fatal(err)
	}
	native := &fakeNativeUI{}
	lifecycle := testLifecycle(native)
	lifecycle.TouchNode = node

	if err := lifecycle.Enter(context.Background()); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	written, err := os.ReadFile(node)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != "unlock" {
		t.Fatalf("touch node received %q, want %q", written, "unlock")
	}
}

func TestEnterSkipsTouchUnlockWhenTheNodeIsAbsent(t *testing.T) {
	native := &fakeNativeUI{}
	lifecycle := testLifecycle(native)
	lifecycle.TouchNode = filepath.Join(t.TempDir(), "absent")

	if err := lifecycle.Enter(context.Background()); err != nil {
		t.Fatalf("a missing touch node must not fail entry: %v", err)
	}
	if !native.holding() {
		t.Fatal("the panel was not held")
	}
}

// TestEnterHoldsThePanelAndMarksExclusive covers what entry now consists of.
// Nothing is stopped: the native interface stays alive underneath, which is
// exactly why the touch stream has to be taken (see Exclusive).
func TestEnterHoldsThePanelAndMarksExclusive(t *testing.T) {
	native := &fakeNativeUI{}
	lifecycle := testLifecycle(native)

	if lifecycle.Exclusive() {
		t.Fatal("a fresh lifecycle reported exclusive before entry")
	}
	if err := lifecycle.Enter(context.Background()); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	if got := native.sequence(); !equalSequence(got, []string{"hold", "capture"}) {
		t.Fatalf("entry issued %v, want exactly [hold capture]", got)
	}
	if !lifecycle.Exclusive() {
		t.Fatal("entry did not mark the device exclusive")
	}
}

// TestExitRestoresTheCapturedPanel is the whole recovery. The panel is not
// given back by starting anything, and not by asking the interface to redraw:
// on this firmware it never learns its pixels were overwritten. Putting the
// captured frame back is what actually returns the reader to where they were.
func TestExitRestoresTheCapturedPanel(t *testing.T) {
	native := &fakeNativeUI{}
	lifecycle := testLifecycle(native)
	if err := lifecycle.Enter(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := lifecycle.Exit(context.Background()); err != nil {
		t.Fatalf("Exit: %v", err)
	}
	if got := native.sequence(); !equalSequence(got, []string{"hold", "capture", "release", "restore", "discard"}) {
		t.Fatalf("exit issued %v, want [hold capture release restore discard]", got)
	}
	if lifecycle.Exclusive() {
		t.Fatal("exit left the device marked exclusive")
	}
	if native.holding() {
		t.Fatal("exit left the panel held")
	}
}

// TestExitDropsExclusiveBeforeRestoringThePanel pins the ordering. If the
// restore landed first the interface would come back while the touch stream was
// still grabbed, i.e. visible but untouchable — not a recovered device.
func TestExitDropsExclusiveBeforeRestoringThePanel(t *testing.T) {
	native := &fakeNativeUI{}
	lifecycle := testLifecycle(native)
	if err := lifecycle.Enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	exclusiveDuringRestore := true
	lifecycle.Snapshot = &observingSnapshot{inner: lifecycle.Snapshot, onRestore: func() {
		exclusiveDuringRestore = lifecycle.Exclusive()
	}}

	if err := lifecycle.Exit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exclusiveDuringRestore {
		t.Fatal("the panel was restored while the touch stream was still held exclusively")
	}
}

type observingSnapshot struct {
	inner     PanelSnapshot
	onRestore func()
}

func (o *observingSnapshot) Capture(ctx context.Context) error { return o.inner.Capture(ctx) }
func (o *observingSnapshot) Owed() bool                        { return o.inner.Owed() }
func (o *observingSnapshot) Discard() error                    { return o.inner.Discard() }
func (o *observingSnapshot) Restore(ctx context.Context) error {
	if o.onRestore != nil {
		o.onRestore()
	}
	return o.inner.Restore(ctx)
}

// TestExitIsIdempotentAndSafeWithoutEntry covers the two calls the product
// actually makes most often: a second exit from a queued caller, and an exit on
// a device that never entered exclusive mode at all.
func TestExitIsIdempotentAndSafeWithoutEntry(t *testing.T) {
	native := &fakeNativeUI{}
	lifecycle := testLifecycle(native)

	if err := lifecycle.Exit(context.Background()); err != nil {
		t.Fatalf("exit without entry: %v", err)
	}
	if err := lifecycle.Exit(context.Background()); err != nil {
		t.Fatalf("second exit: %v", err)
	}
	// Nothing was ever covered, so nothing is owed and nothing is restored.
	// This is the path a Guardian that only supervised the service takes on
	// shutdown, and the path a REST exit takes on a device that is not in
	// exclusive mode; reporting a failed recovery there would be a lie.
	if got := native.sequence(); !equalSequence(got, []string{"release", "release"}) {
		t.Fatalf("issued %v, want two releases and no restore", got)
	}
	if lifecycle.Exclusive() {
		t.Fatal("exit marked the device exclusive")
	}
}

// TestExitAttemptsTheRestoreEvenWhenReleaseFails is the reason the two steps
// are not chained: a screensaver policy that will not move must not cost the
// user the panel, because the restored frame is the part they can see.
func TestExitAttemptsTheRestoreEvenWhenReleaseFails(t *testing.T) {
	native := &fakeNativeUI{}
	lifecycle := testLifecycle(native)
	if err := lifecycle.Enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	native.freeErr = errors.New("powerd busy")

	if err := lifecycle.Exit(context.Background()); !errors.Is(err, ErrLifecycle) {
		t.Fatalf("Exit error = %v, want ErrLifecycle", err)
	}
	if got := native.sequence(); !equalSequence(got, []string{"hold", "capture", "release", "restore", "discard"}) {
		t.Fatalf("issued %v, want the restore to have been attempted anyway", got)
	}
}

func TestExitReportsAFailedRestore(t *testing.T) {
	native := &fakeNativeUI{}
	lifecycle := &Lifecycle{Native: native, Snapshot: &fakeSnapshot{native: native, restoreErr: errors.New("panel refused")}, Timeout: 25 * time.Millisecond}
	if err := lifecycle.Enter(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := lifecycle.Exit(context.Background()); !errors.Is(err, ErrLifecycle) {
		t.Fatalf("Exit error = %v, want ErrLifecycle", err)
	}
}

// TestEnterRefusesWhenThePanelCannotBeCaptured is the invariant that this whole
// redesign exists to establish: the panel is only ever taken by a service that
// has already proved it can hand it back. Entering without a capture would
// recreate exactly the failure that stranded the device before.
func TestEnterRefusesWhenThePanelCannotBeCaptured(t *testing.T) {
	native := &fakeNativeUI{}
	lifecycle := &Lifecycle{
		Native:   native,
		Snapshot: &fakeSnapshot{native: native, captureErr: errors.New("framebuffer unreadable")},
		Timeout:  25 * time.Millisecond,
	}

	if err := lifecycle.Enter(context.Background()); !errors.Is(err, ErrLifecycle) {
		t.Fatalf("Enter error = %v, want ErrLifecycle", err)
	}
	if lifecycle.Exclusive() {
		t.Fatal("the panel was taken despite an uncapturable frame")
	}
	if native.holding() {
		t.Fatal("the failed entry left the panel held")
	}
	if got := native.sequence(); !equalSequence(got, []string{"hold", "capture", "release"}) {
		t.Fatalf("issued %v, want a failed capture followed by a rollback with nothing owed", got)
	}
}

// TestEnterFailureRollsBackToTheNativeInterface keeps the invariant that a
// half-exclusive device is never a resting state.
func TestEnterFailureRollsBackToTheNativeInterface(t *testing.T) {
	native := &fakeNativeUI{holdErr: errors.New("powerd unavailable")}
	lifecycle := testLifecycle(native)

	if err := lifecycle.Enter(context.Background()); !errors.Is(err, ErrLifecycle) {
		t.Fatalf("Enter error = %v, want ErrLifecycle", err)
	}
	if got := native.sequence(); !equalSequence(got, []string{"hold", "release"}) {
		t.Fatalf("issued %v, want a failed hold followed by a rollback with nothing owed", got)
	}
	if lifecycle.Exclusive() {
		t.Fatal("a failed entry left the device marked exclusive")
	}
}

// TestEnterRollsBackOnAnAlreadyCancelledContext proves the rollback runs on its
// own deadline: the caller's context is already dead when Enter is reached.
func TestEnterRollsBackOnAnAlreadyCancelledContext(t *testing.T) {
	native := &fakeNativeUI{holdErr: errors.New("powerd unavailable")}
	lifecycle := testLifecycle(native)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := lifecycle.Enter(ctx); !errors.Is(err, ErrLifecycle) {
		t.Fatalf("Enter error = %v, want ErrLifecycle", err)
	}
	if got := native.sequence(); !equalSequence(got, []string{"hold", "release"}) {
		t.Fatalf("issued %v, want the rollback to have run on its own deadline", got)
	}
}

// TestLipcNativeUIWritesTheVerifiedProperties pins the exact lipc writes. They
// are not interchangeable with the pillow-era ones: `lipc-set-prop` exits 0 for
// a property that does not exist on this firmware, so these were chosen by
// comparing framebuffer contents on a device, and displayChrome,
// interrogatePillow and appmgrd's startdefault all failed that test.
func TestLipcNativeUIWritesTheVerifiedProperties(t *testing.T) {
	var issued [][]string
	native := &LipcNativeUI{Run: func(_ context.Context, name string, args ...string) error {
		issued = append(issued, append([]string{name}, args...))
		return nil
	}}
	ctx := context.Background()
	if err := native.HoldPanel(ctx); err != nil {
		t.Fatal(err)
	}
	if err := native.ReleasePanel(ctx); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"lipc-set-prop", "com.lab126.powerd", "preventScreenSaver", "1"},
		{"lipc-set-prop", "com.lab126.powerd", "preventScreenSaver", "0"},
	}
	if len(issued) != len(want) {
		t.Fatalf("issued %v, want %v", issued, want)
	}
	for index := range want {
		if !equalSequence(issued[index], want[index]) {
			t.Fatalf("issued %v, want %v", issued, want)
		}
	}
}

// TestNoNativeUnitIsEverStoppedOrStarted is the guard on the retired design.
// Stopping `lab126_gui` and `framework` is what made exclusive mode
// unrecoverable, and the upstart account of those jobs is what made the failure
// invisible. Neither verb may come back.
func TestNoNativeUnitIsEverStoppedOrStarted(t *testing.T) {
	var issued []string
	native := &LipcNativeUI{Run: func(_ context.Context, name string, args ...string) error {
		issued = append(issued, name)
		return nil
	}}
	lifecycle := &Lifecycle{Native: native, Timeout: time.Second, TouchNode: filepath.Join(t.TempDir(), "absent")}
	if err := lifecycle.Enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Exit(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range issued {
		switch name {
		case "stop", "start", "restart", "status", "initctl":
			t.Fatalf("exclusive mode issued %q; native units must never be stopped or started again", name)
		}
	}
	if len(issued) == 0 {
		t.Fatal("no lipc command was issued at all")
	}
}

func TestActivityMatchesCompletedLifecycleRecovery(t *testing.T) {
	native := &fakeNativeUI{}
	lifecycle := testLifecycle(native)
	path := filepath.Join(t.TempDir(), "activity.json")

	if err := lifecycle.Exit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := StoreActivity(path, Activity{Reason: exitReasonGesture, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	activity := LoadActivity(path)
	if activity.Active || activity.Failsafe || activity.Reason != exitReasonGesture {
		t.Fatalf("activity = %+v, want deliberate inactive recovery", activity)
	}
	if native.holding() {
		t.Fatal("activity was recorded inactive while the panel was still held")
	}
}

// The three tests below pin the debt-token semantics of the panel snapshot.
// Each of them failed when first written: the snapshot had been designed as a
// cache, and a cache is the wrong model for "we owe this reader their screen".

// TestExitWithoutEverEnteringOwesNothing covers the Guardian that only ever
// supervised the service — a fresh install, or any device left inactive by a
// previous exit. It never covered the panel, so it owes no frame, and its exit
// must not report a failed recovery. Before the fix this returned a 500 from
// POST /v1/system/exit and made a signalled shutdown exit non-zero.
func TestExitWithoutEverEnteringOwesNothing(t *testing.T) {
	snapshot := &FramebufferSnapshot{Path: filepath.Join(t.TempDir(), snapshotName), Panel: &recordingPanel{}}
	lifecycle := &Lifecycle{
		Native:   &LipcNativeUI{Run: func(context.Context, string, ...string) error { return nil }},
		Snapshot: snapshot,
		Timeout:  time.Second,
	}
	if err := lifecycle.Exit(context.Background()); err != nil {
		t.Fatalf("an exit that never covered the panel reported %v", err)
	}
}

// TestReEntryNeverOverwritesAnOwedFrame is the one that actually strands a
// device. A Guardian killed while the panel is covered leaves the debt
// outstanding; by the time the next one starts, the framebuffer holds *our*
// content. Capturing again would replace the native frame we owe with the very
// thing we owe it for, and the reader would never get their screen back.
func TestReEntryNeverOverwritesAnOwedFrame(t *testing.T) {
	dir := t.TempDir()
	native := fakeFramebuffer(t, 8, 4, 8, 8, func(int, int) uint8 { return 0x11 })
	snapshot := &FramebufferSnapshot{
		Path:        filepath.Join(dir, snapshotName),
		Framebuffer: native,
		Stride:      sysfsValue(t, "stride", "8"),
		Depth:       sysfsValue(t, "bits_per_pixel", "8"),
		Probe:       &FakeScreen{Capabilities: ScreenCapabilities{Width: 8, Height: 4}},
		Panel:       &recordingPanel{},
	}
	newLifecycle := func() *Lifecycle {
		return &Lifecycle{
			Native:    &LipcNativeUI{Run: func(context.Context, string, ...string) error { return nil }},
			Snapshot:  snapshot,
			Timeout:   time.Second,
			TouchNode: filepath.Join(dir, "absent"),
		}
	}
	if err := newLifecycle().Enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	owed := string(readFile(t, snapshot.Path))

	// The Guardian is killed. The panel now shows our content, and a fresh
	// Guardian re-enters from the still-active activity record.
	snapshot.Framebuffer = fakeFramebuffer(t, 8, 4, 8, 8, func(int, int) uint8 { return 0xee })
	if err := newLifecycle().Enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, snapshot.Path)); got != owed {
		t.Fatal("re-entry overwrote the owed native frame with the content already on the panel")
	}
}

// TestCompletedExitSettlesTheDebt keeps a settled debt from being paid twice:
// a token that outlives its restore would make the next exit replay a frame
// that stopped describing the interface long ago.
func TestCompletedExitSettlesTheDebt(t *testing.T) {
	dir := t.TempDir()
	snapshot := &FramebufferSnapshot{
		Path:        filepath.Join(dir, snapshotName),
		Framebuffer: fakeFramebuffer(t, 8, 4, 8, 8, func(int, int) uint8 { return 0x11 }),
		Stride:      sysfsValue(t, "stride", "8"),
		Depth:       sysfsValue(t, "bits_per_pixel", "8"),
		Probe:       &FakeScreen{Capabilities: ScreenCapabilities{Width: 8, Height: 4}},
		Panel:       &recordingPanel{},
	}
	lifecycle := &Lifecycle{
		Native:    &LipcNativeUI{Run: func(context.Context, string, ...string) error { return nil }},
		Snapshot:  snapshot,
		Timeout:   time.Second,
		TouchNode: filepath.Join(dir, "absent"),
	}
	if err := lifecycle.Enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !snapshot.Owed() {
		t.Fatal("entry did not record the debt")
	}
	if err := lifecycle.Exit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snapshot.Owed() {
		t.Fatal("a completed exit left the debt token behind")
	}
}

// TestFailedRestoreKeepsTheDebtOutstanding is the property that makes the token
// worth having: a recovery that did not reach the panel stays owed, so the next
// exit — from a retry, the corner gesture or the Guardian shutting down — tries
// again instead of quietly forgetting.
func TestFailedRestoreKeepsTheDebtOutstanding(t *testing.T) {
	dir := t.TempDir()
	panel := &recordingPanel{err: errors.New("fbink failed")}
	snapshot := &FramebufferSnapshot{
		Path:        filepath.Join(dir, snapshotName),
		Framebuffer: fakeFramebuffer(t, 8, 4, 8, 8, func(int, int) uint8 { return 0x11 }),
		Stride:      sysfsValue(t, "stride", "8"),
		Depth:       sysfsValue(t, "bits_per_pixel", "8"),
		Probe:       &FakeScreen{Capabilities: ScreenCapabilities{Width: 8, Height: 4}},
		Panel:       panel,
	}
	lifecycle := &Lifecycle{
		Native:    &LipcNativeUI{Run: func(context.Context, string, ...string) error { return nil }},
		Snapshot:  snapshot,
		Timeout:   time.Second,
		TouchNode: filepath.Join(dir, "absent"),
	}
	if err := lifecycle.Enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Exit(context.Background()); !errors.Is(err, ErrLifecycle) {
		t.Fatalf("Exit error = %v, want ErrLifecycle", err)
	}
	if !snapshot.Owed() {
		t.Fatal("a failed restore settled the debt anyway; the panel would never be handed back")
	}
	// A later attempt succeeds and settles it.
	panel.err = nil
	if err := lifecycle.Exit(context.Background()); err != nil {
		t.Fatalf("the retry reported %v", err)
	}
	if snapshot.Owed() {
		t.Fatal("the successful retry did not settle the debt")
	}
}
