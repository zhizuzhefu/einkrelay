package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// ErrLifecycle is the single opaque failure returned by every exclusive-mode
// operation. Callers only ever need to know that the device did not reach the
// requested state; the underlying command output stays out of responses.
var ErrLifecycle = errors.New("lifecycle operation failed")

// NativeUI is the port for the native Kindle interface.
//
// It deliberately does not stop or start anything. An earlier design took the
// panel by stopping the `lab126_gui` and `framework` upstart jobs and gave it
// back by starting them again, which turned out to be the wrong model on this
// firmware in two independent ways.
//
// First, the jobs are not a truthful account of the interface. `status
// lab126_gui` reports `start/running` with no process behind it, so every
// readback the old code performed was reading bookkeeping rather than reality,
// and a recovery that had restored nothing still reported success.
//
// Second, and decisively: on 5.18 the interface is an event-driven stack —
// Xorg, the awesome window manager and the blanket framework daemon hosting
// com.lab126.KPPMainApp. It repaints when something happens, not on a timer.
// Writing a frame to the framebuffer leaves it with no idea its pixels are
// gone, so it never repaints, and no amount of cycling a job fixes that. This
// was demonstrated with nothing stopped at all: draw one frame with the whole
// native stack alive and healthy, and the panel stays on that frame forever.
//
// So exclusive mode is three much smaller things: hold the panel awake, take
// the touch stream so taps do not reach the interface underneath, and — since
// the interface cannot be made to redraw itself — put the pixels back by hand
// on the way out. See PanelSnapshot for why that is the recovery.
type NativeUI interface {
	// HoldPanel keeps the screensaver from replacing the displayed frame.
	HoldPanel(ctx context.Context) error
	// ReleasePanel returns the screensaver policy to the system default.
	ReleasePanel(ctx context.Context) error
}

type commandRunner func(ctx context.Context, name string, args ...string) error

func execRunner(ctx context.Context, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return ErrLifecycle
	}
	return nil
}

// LipcNativeUI drives the Kindle power daemon over lipc. The power button
// itself is never touched; only the screensaver policy and the wake signal.
//
// `lipc-set-prop` exits 0 for a property that does not exist on the running
// firmware, so its exit status is not evidence that anything happened. Only
// preventScreenSaver survived being checked against the device, and it is the
// only property written here. In particular powerd's wakeUp is not used: it
// does repaint the interface, but only while the device is asleep — awake it
// answers lipcErrNoSuchProperty, and awake is the state every exit runs in.
type LipcNativeUI struct {
	Run commandRunner
}

func (n *LipcNativeUI) runner() commandRunner {
	if n.Run != nil {
		return n.Run
	}
	return execRunner
}

func (n *LipcNativeUI) HoldPanel(ctx context.Context) error {
	return n.runner()(ctx, "lipc-set-prop", "com.lab126.powerd", "preventScreenSaver", "1")
}

func (n *LipcNativeUI) ReleasePanel(ctx context.Context) error {
	return n.runner()(ctx, "lipc-set-prop", "com.lab126.powerd", "preventScreenSaver", "0")
}

var _ NativeUI = (*LipcNativeUI)(nil)

// Lifecycle owns entry into and exit from exclusive mode. Both directions are
// idempotent: every step is a property write that states the desired end state
// rather than a transition, so a repeated call, a timeout or a partial failure
// converges on either a clean active state or the native interface.
type Lifecycle struct {
	Native NativeUI
	// Snapshot preserves the native interface's own frame across exclusive
	// mode. Entry refuses to proceed when it cannot be captured: taking a panel
	// that cannot be handed back is the failure this whole redesign exists to
	// remove.
	Snapshot PanelSnapshot
	Timeout  time.Duration
	// TouchNode is the goodix touch lock node (/proc/touch on the PW4). The
	// controller can be left in a locked sleep with the event stream at zero;
	// unlocking is harmless when it is already unlocked. Empty disables the
	// step.
	TouchNode string

	mu        sync.Mutex
	exclusive atomic.Bool
	// changes carries exclusivity transitions to whoever is holding the touch
	// stream. Polling for them would mean either a wakeup every few tens of
	// milliseconds on a battery-powered device, or a window in which the panel
	// has been handed back and taps still are not reaching the interface the
	// reader can see. A single buffered slot is enough: the receiver only ever
	// needs to know that the state moved, and then reads it.
	changes chan struct{}
}

func NewLifecycle(native NativeUI, snapshot PanelSnapshot, timeout time.Duration) *Lifecycle {
	return &Lifecycle{Native: native, Snapshot: snapshot, Timeout: timeout, changes: make(chan struct{}, 1)}
}

// ExclusiveChanged fires whenever Exclusive flips. It never blocks the
// lifecycle: a pending notification the receiver has not consumed yet already
// says everything the next one would.
func (l *Lifecycle) ExclusiveChanged() <-chan struct{} { return l.changes }

func (l *Lifecycle) notifyExclusiveChange() {
	if l.changes == nil {
		return
	}
	select {
	case l.changes <- struct{}{}:
	default:
	}
}

func (l *Lifecycle) timeout() time.Duration {
	if l.Timeout <= 0 {
		return 20 * time.Second
	}
	return l.Timeout
}

// Exclusive reports whether the panel is currently held. The gesture watcher
// reads it to decide whether the touch stream should be taken exclusively:
// while the native interface is visible it must keep receiving taps, and while
// it is covered it must not.
func (l *Lifecycle) Exclusive() bool { return l.exclusive.Load() }

// Enter takes the panel. Any failure rolls all the way back to the native
// interface: a half-exclusive device is never an acceptable resting state, so
// the rollback runs on its own deadline even when the caller's context has
// already expired.
func (l *Lifecycle) Enter(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	scoped, cancel := context.WithTimeout(ctx, l.timeout())
	defer cancel()
	if err := l.Native.HoldPanel(scoped); err != nil {
		return l.rollback()
	}
	// The frame the reader is looking at is captured before anything covers it.
	// A failure here is a refusal to enter: the panel is only ever taken by a
	// service that has already proved it can give it back.
	//
	// An already-outstanding debt is left exactly as it is. That is the case
	// where a previous Guardian was killed while the panel was covered: what is
	// on the framebuffer now is our own content, so capturing would overwrite
	// the native frame we still owe with the very thing we owe it for, and the
	// reader would never see their screen again.
	if l.Snapshot != nil && !l.Snapshot.Owed() {
		if err := l.Snapshot.Capture(scoped); err != nil {
			return l.rollback()
		}
	}
	l.unlockTouch()
	l.exclusive.Store(true)
	l.notifyExclusiveChange()
	return nil
}

// unlockTouch releases the goodix touch controller from a locked sleep. A
// missing node means a different controller — nothing to do, and never a
// reason to roll back an otherwise clean entry.
func (l *Lifecycle) unlockTouch() {
	node := l.TouchNode
	if node == "" {
		node = "/proc/touch"
	}
	if _, err := os.Stat(node); err != nil {
		return
	}
	_ = os.WriteFile(node, []byte("unlock"), 0)
}

// Exit gives the panel back. It is the only recovery path in the product and
// is safe to call repeatedly, including when exclusive mode was never entered.
func (l *Lifecycle) Exit(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	scoped, cancel := context.WithTimeout(ctx, l.timeout())
	defer cancel()
	return l.exitLocked(scoped)
}

func (l *Lifecycle) exitLocked(ctx context.Context) error {
	// The flag drops first so the gesture watcher stops holding the touch
	// stream before the interface is asked to redraw: an interface that comes
	// back and cannot be touched is not a recovered device.
	l.exclusive.Store(false)
	l.notifyExclusiveChange()
	// Both steps are attempted even if the first fails, because the restore is
	// what the user actually sees and a stuck screensaver policy must not cost
	// them the panel.
	failed := l.Native.ReleasePanel(ctx) != nil
	// Nothing owed means nothing was ever covered — a Guardian that only ever
	// supervised the service still leaves through here, and reporting a failed
	// recovery for a panel it never took would be a lie that costs the caller a
	// 500 and the shutdown a non-zero exit status.
	if l.Snapshot != nil && l.Snapshot.Owed() {
		if err := l.Snapshot.Restore(ctx); err != nil {
			failed = true
		} else if l.Snapshot.Discard() != nil {
			// The frame is back where it belongs, which is what the caller
			// asked for; a debt token that outlived its debt is a bookkeeping
			// problem, not a reason to report the recovery as failed.
			failed = failed || false
		}
	}
	if failed {
		return ErrLifecycle
	}
	return nil
}

// rollback performs a full Exit on a fresh deadline and reports the original
// failure. It is only ever called with l.mu already held.
func (l *Lifecycle) rollback() error {
	recovery, cancel := context.WithTimeout(context.Background(), l.timeout())
	defer cancel()
	_ = l.exitLocked(recovery)
	return ErrLifecycle
}

// Activity is the persistent record of whether the device should be in
// exclusive mode. A deliberate exit clears Active; a crash or a reboot does not,
// which is what lets the Guardian tell "the user asked to leave" apart from
// "the service died".
type Activity struct {
	Active   bool      `json:"active"`
	Failsafe bool      `json:"failsafe"`
	Reason   string    `json:"reason"`
	At       time.Time `json:"at"`
}

// LoadActivity fails closed: an unreadable or corrupt record reads as inactive,
// so a damaged state file can never cause an unrequested re-entry into
// exclusive mode.
func LoadActivity(path string) Activity {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Activity{}
	}
	var activity Activity
	if json.Unmarshal(raw, &activity) != nil {
		return Activity{}
	}
	return activity
}

// StoreActivity writes the record atomically. The rename plus directory fsync
// is what makes the transition observable-or-not rather than half-written when
// the battery is pulled.
func StoreActivity(path string, activity Activity) error {
	payload, err := json.Marshal(activity)
	if err != nil {
		return ErrLifecycle
	}
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "activity-*.tmp")
	if err != nil {
		return ErrLifecycle
	}
	name := file.Name()
	abandon := func() error {
		_ = file.Close()
		_ = os.Remove(name)
		return ErrLifecycle
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
		return ErrLifecycle
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return ErrLifecycle
	}
	if err := syncDir(dir); err != nil {
		return ErrLifecycle
	}
	return nil
}
