package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrFailsafe reports that the supervisor stopped restarting the service. It is
// only ever returned so the caller can hand the panel back to the native UI.
var ErrFailsafe = errors.New("failsafe tripped")

// ErrGuardian is the opaque failure of a Guardian control-socket request.
var ErrGuardian = errors.New("guardian request failed")

// Reasons recorded in the activity file. They are the only vocabulary the
// persisted record uses, so a later start can tell a deliberate exit, a
// failsafe and a crash apart.
const (
	exitReasonREST     = "rest_exit"
	exitReasonGesture  = "corner_triple_tap"
	exitReasonFailsafe = "failsafe"
	exitReasonEnter    = "enter_failed"
	resumeReason       = "resume"
	// recoveryPendingReason is durable intent, not a mode.  It makes an
	// interrupted restore converge towards the native UI on the next Guardian
	// start instead of treating the old Active bit as permission to re-enter
	// exclusive mode.
	recoveryPendingReason = "recovery_pending"
)

// The control protocol is one verb per connection, terminated by a newline.
// Deliberately not HTTP: the path the service depends on for recovery should
// carry no router, no parser and no dependency on the service being healthy.
const (
	guardianExitCommand = "EXIT"
	guardianOK          = "OK"
	guardianError       = "ERR"
	guardianMaxLine     = 64
)

// GuardianClient is how the service process asks the Guardian to leave
// exclusive mode. It has no local fallback on purpose: if the Guardian is not
// listening the call fails fast, because a second recovery implementation
// racing the real one is exactly how a device ends up half-exclusive.
type GuardianClient struct {
	Path    string
	Timeout time.Duration
}

func (c *GuardianClient) timeout() time.Duration {
	if c.Timeout <= 0 {
		return 20 * time.Second
	}
	return c.Timeout
}

func (c *GuardianClient) ExitExclusive(ctx context.Context) error {
	scoped, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(scoped, "unix", c.Path)
	if err != nil {
		return ErrGuardian
	}
	defer conn.Close()
	if deadline, ok := scoped.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := io.WriteString(conn, guardianExitCommand+"\n"); err != nil {
		return ErrGuardian
	}
	reply, err := bufio.NewReader(io.LimitReader(conn, guardianMaxLine)).ReadString('\n')
	if err != nil || strings.TrimSpace(reply) != guardianOK {
		return ErrGuardian
	}
	return nil
}

var _ NativeController = (*GuardianClient)(nil)

// GuardianServer exposes the single recovery verb. Connections are handled one
// at a time, which is what keeps concurrent exit requests from overlapping.
type GuardianServer struct {
	Path    string
	Exit    func(context.Context, string) error
	Timeout time.Duration

	mu       sync.Mutex
	listener net.Listener
}

func (s *GuardianServer) timeout() time.Duration {
	if s.Timeout <= 0 {
		return 20 * time.Second
	}
	return s.Timeout
}

// Listen binds the socket inside the 0700 state directory. A stale node left by
// a killed Guardian is removed first, but only when it really is a socket, so a
// misconfigured path can never delete the token or a saved screen.
func (s *GuardianServer) Listen() error {
	if s.Path == "" {
		return ErrGuardian
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
		return ErrGuardian
	}
	if info, err := os.Lstat(s.Path); err == nil && info.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(s.Path)
	}
	listener, err := net.Listen("unix", s.Path)
	if err != nil {
		return ErrGuardian
	}
	if err := os.Chmod(s.Path, 0600); err != nil {
		_ = listener.Close()
		return ErrGuardian
	}
	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()
	return nil
}

func (s *GuardianServer) current() net.Listener {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listener
}

func (s *GuardianServer) Serve(ctx context.Context) {
	listener := s.current()
	if listener == nil {
		return
	}
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		s.handle(ctx, conn)
	}
}

func (s *GuardianServer) Close() error {
	s.mu.Lock()
	listener := s.listener
	s.listener = nil
	s.mu.Unlock()
	if listener == nil {
		return nil
	}
	return listener.Close()
}

func (s *GuardianServer) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	// The read, the recovery and the reply share the connection deadline, so a
	// wedged peer cannot hold the only recovery path open indefinitely.
	_ = conn.SetDeadline(time.Now().Add(2 * s.timeout()))
	line, err := bufio.NewReader(io.LimitReader(conn, guardianMaxLine)).ReadString('\n')
	if err != nil {
		return
	}
	if strings.TrimSpace(line) != guardianExitCommand || s.Exit == nil {
		_, _ = io.WriteString(conn, guardianError+"\n")
		return
	}
	// Recovery is detached from the accept context: once asked to leave
	// exclusive mode, the Guardian finishes restoring the native UI even if it
	// is being shut down at the same moment.
	scoped, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout())
	defer cancel()
	if s.Exit(scoped, exitReasonREST) != nil {
		_, _ = io.WriteString(conn, guardianError+"\n")
		return
	}
	_, _ = io.WriteString(conn, guardianOK+"\n")
}

// exclusiveLifecycle is the recovery half of Lifecycle. Naming it separately
// keeps the coordinator testable without a device.
type exclusiveLifecycle interface {
	Exit(context.Context) error
}

// ExitCoordinator is the single deliberate-exit path. The REST endpoint (over
// the control socket) and the corner long-press both land here, which is what
// makes a repeated exit idempotent and keeps recovery available when the
// service process has crashed. Concurrent triggers share ONE recovery
// execution (singleflight): a second caller never starts a competing exit
// mid-flight — it waits and receives the same result.
type ExitCoordinator struct {
	Lifecycle    exclusiveLifecycle
	ActivityPath string
	Now          func() time.Time
	Store        func(string, Activity) error
	mu           sync.Mutex
	inFlight     bool
	done         chan struct{}
	flightError  error
}

func (c *ExitCoordinator) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

func (c *ExitCoordinator) store(activity Activity) error {
	if c.Store != nil {
		return c.Store(c.ActivityPath, activity)
	}
	return StoreActivity(c.ActivityPath, activity)
}

// Exit records the deliberate exit before performing it. The order matters: if
// the device dies between the two steps the worst case is one extra idempotent
// recovery on the next start, whereas the reverse order could re-enter
// exclusive mode immediately after the user asked to leave.
func (c *ExitCoordinator) Exit(ctx context.Context, reason string) error {
	c.mu.Lock()
	if c.inFlight {
		done := c.done
		c.mu.Unlock()
		<-done
		c.mu.Lock()
		err := c.flightError
		c.mu.Unlock()
		return err
	}
	c.inFlight = true
	c.done = make(chan struct{})
	c.mu.Unlock()

	err := c.executeExit(ctx, reason)

	c.mu.Lock()
	c.flightError = err
	c.inFlight = false
	close(c.done)
	c.mu.Unlock()
	return err
}

func (c *ExitCoordinator) executeExit(ctx context.Context, reason string) error {
	// Persist the recovery intent first.  If the process or lifecycle dies in
	// the middle of Exit, a later Guardian will retry restoration rather than
	// re-entering exclusive mode from a stale Active record.
	pending := Activity{Active: true, Failsafe: reason == exitReasonFailsafe, Reason: recoveryPendingReason, At: c.now()}
	_ = c.store(pending)
	// There is no longer a goodbye screen. It existed because recovery used to
	// take tens of seconds (stopping and restarting the native units), during
	// which a frozen content page read like a dead device. Recovery is now two
	// property writes and completes faster than a page could be rendered, so a
	// "please wait" screen would cost more time than it explains and would
	// promise a delay that no longer happens.
	//
	// The recovery runs even when intent could not be written: the native UI
	// always takes priority over bookkeeping.
	exitErr := c.Lifecycle.Exit(ctx)
	if exitErr != nil {
		return ErrLifecycle
	}
	// Only a completed restore clears the activity latch.  A failed final
	// store leaves recovery_pending behind for the next Guardian invocation.
	record := Activity{Failsafe: reason == exitReasonFailsafe, Reason: reason, At: c.now()}
	if c.store(record) != nil {
		return ErrLifecycle
	}
	// A failed intent write does not prevent a successful completed-recovery
	// record from converging a previously active installation to inactive.
	return nil
}

// shouldEnterExclusive is the whole activity-record policy. A crash or a reboot
// leaves Active set and exclusive mode resumes; a deliberate exit clears it; a
// failsafe additionally latches until an explicit resume.
func shouldEnterExclusive(activity Activity) bool {
	return activity.Active && !activity.Failsafe && activity.Reason != recoveryPendingReason
}

// recoverPending restores the native UI before the Guardian makes any entry
// decision. It deliberately routes through ExitCoordinator so the final
// activity state is committed only after a successful, idempotent Exit.
func recoverPending(ctx context.Context, coordinator *ExitCoordinator) error {
	pending := LoadActivity(coordinator.ActivityPath)
	if pending.Reason != recoveryPendingReason {
		return nil
	}
	if pending.Failsafe {
		return coordinator.Exit(ctx, exitReasonFailsafe)
	}
	return coordinator.Exit(ctx, exitReasonREST)
}

// ServiceRunner starts one service process and blocks until it has exited.
type ServiceRunner interface {
	Run(ctx context.Context) error
}

// SubprocessRunner runs a subcommand of this same executable. The Guardian's
// lifetime is independent of it: the child is a separate process and its death
// is an ordinary iteration of the supervision loop.
type SubprocessRunner struct {
	Executable string
	Args       []string
	Stderr     io.Writer
	GraceDelay time.Duration
}

func (r *SubprocessRunner) grace() time.Duration {
	if r.GraceDelay <= 0 {
		return 5 * time.Second
	}
	return r.GraceDelay
}

func (r *SubprocessRunner) Run(ctx context.Context) error {
	command := exec.CommandContext(ctx, r.Executable, r.Args...)
	command.Stdout = io.Discard
	command.Stderr = r.Stderr
	if command.Stderr == nil {
		command.Stderr = io.Discard
	}
	// The service is asked to shut down before it is killed, so an in-flight
	// display transaction still gets to finish or abandon its candidate file.
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = r.grace()
	return command.Run()
}

var _ ServiceRunner = (*SubprocessRunner)(nil)

// defaultSupervisorBackoff spends 1+2+4+8 = 15 seconds of backoff across five
// start attempts, which keeps a full crash loop comfortably inside the 60
// second failure window rather than sliding out of it.
var defaultSupervisorBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

// Supervisor restarts the service with a bounded backoff and trips the failsafe
// when it keeps failing to start. "Failing to start" is deliberately defined as
// exiting sooner than HealthyAfter: a service that came up, served for a while
// and then died is a restart, not a start failure.
type Supervisor struct {
	Runner        ServiceRunner
	Backoff       []time.Duration
	HealthyAfter  time.Duration
	FailureWindow time.Duration
	MaxFailures   int
	Now           func() time.Time
	Sleep         func(context.Context, time.Duration) error
	Log           func(string)
}

func (s *Supervisor) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Supervisor) log(message string) {
	if s.Log != nil {
		s.Log(message)
	}
}

func (s *Supervisor) healthyAfter() time.Duration {
	if s.HealthyAfter <= 0 {
		return 10 * time.Second
	}
	return s.HealthyAfter
}

func (s *Supervisor) failureWindow() time.Duration {
	if s.FailureWindow <= 0 {
		return time.Minute
	}
	return s.FailureWindow
}

func (s *Supervisor) maxFailures() int {
	if s.MaxFailures < 1 {
		return 5
	}
	return s.MaxFailures
}

func (s *Supervisor) backoff(attempt int) time.Duration {
	schedule := s.Backoff
	if len(schedule) == 0 {
		schedule = defaultSupervisorBackoff
	}
	if attempt >= len(schedule) {
		attempt = len(schedule) - 1
	}
	if attempt < 0 {
		attempt = 0
	}
	return schedule[attempt]
}

func (s *Supervisor) sleep(ctx context.Context, delay time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Supervise returns nil when the context is cancelled and ErrFailsafe when the
// crash loop has to be broken. It never returns because of a single failure.
func (s *Supervisor) Supervise(ctx context.Context) error {
	failures := make([]time.Time, 0, s.maxFailures())
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		started := s.now()
		err := s.Runner.Run(ctx)
		if ctx.Err() != nil {
			return nil
		}
		finished := s.now()
		if finished.Sub(started) >= s.healthyAfter() {
			s.log("the service exited after a healthy run; restarting")
			failures = failures[:0]
			attempt = 0
		} else {
			// The window slides by dropping timestamps older than the cutoff
			// rather than by subtracting durations, so a long backoff carries
			// old failures out of it instead of accumulating them forever.
			cutoff := finished.Add(-s.failureWindow())
			recent := failures[:0]
			for _, at := range failures {
				if at.After(cutoff) {
					recent = append(recent, at)
				}
			}
			failures = append(recent, finished)
			s.log(fmt.Sprintf("the service failed to start (%d in the window): %v", len(failures), err))
			if len(failures) >= s.maxFailures() {
				return ErrFailsafe
			}
		}
		delay := s.backoff(attempt)
		attempt++
		if s.sleep(ctx, delay) != nil {
			return nil
		}
	}
}

// handleFailsafe applies the failsafe outcome: the native UI comes back and the
// record is latched inactive, which is what makes the Guardian wait for an
// explicit restart instead of re-entering exclusive mode on its own.
func handleFailsafe(ctx context.Context, coordinator *ExitCoordinator, log func(string)) error {
	if log != nil {
		log("the service failed to start repeatedly; restoring the native UI and waiting for an explicit restart")
	}
	return coordinator.Exit(context.WithoutCancel(ctx), exitReasonFailsafe)
}

func runGuard(ctx context.Context, stdout, stderr io.Writer) int {
	cfg, err := LoadConfigFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(stderr, "configuration is invalid")
		return 2
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "the guardian cannot locate its own executable")
		return 1
	}
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		fmt.Fprintln(stderr, "the state directory is not usable")
		return 1
	}
	log := func(message string) { fmt.Fprintln(stderr, "guardian: "+message) }

	snapshot := &FramebufferSnapshot{
		Path:  filepath.Join(cfg.StateDir, snapshotName),
		Probe: &SysfsScreenProbe{},
		Panel: &FBInkPanel{Path: cfg.FBInkPath},
	}
	lifecycle := NewLifecycle(&LipcNativeUI{}, snapshot, cfg.LifecycleTimeout)
	coordinator := &ExitCoordinator{
		Lifecycle:    lifecycle,
		ActivityPath: cfg.ActivityPath,
	}

	activity := LoadActivity(cfg.ActivityPath)
	if activity.Reason == recoveryPendingReason {
		if err := recoverPending(context.WithoutCancel(ctx), coordinator); err != nil {
			log("retrying a pending native UI recovery failed")
		}
		activity = LoadActivity(cfg.ActivityPath)
	}
	if shouldEnterExclusive(activity) {
		if err := lifecycle.Enter(ctx); err != nil {
			log("entering exclusive mode failed; the native UI was restored")
			if StoreActivity(cfg.ActivityPath, Activity{Reason: exitReasonEnter, At: time.Now().UTC()}) != nil {
				log("recording the inactive state failed")
			}
		}
	} else {
		log("exclusive mode was not requested; supervising the service only")
	}

	// Supervision and exclusivity are decoupled. Even when the device is not
	// active the service keeps running, because /v1/status and the idempotent
	// /v1/system/exit have to stay reachable.
	server := &GuardianServer{Path: cfg.GuardianSocket, Exit: coordinator.Exit, Timeout: cfg.LifecycleTimeout}
	if err := server.Listen(); err != nil {
		log("the control socket is unavailable; the REST exit path is disabled")
	} else {
		defer server.Close()
		go server.Serve(ctx)
	}

	stopGesture := startGestureWatcher(ctx, cfg, coordinator, lifecycle.Exclusive, lifecycle.ExclusiveChanged(), log)
	defer stopGesture()

	supervisor := &Supervisor{
		Runner: &SubprocessRunner{Executable: executable, Args: []string{"serve"}, Stderr: stderr},
		Log:    log,
	}
	if errors.Is(supervisor.Supervise(ctx), ErrFailsafe) {
		if err := handleFailsafe(ctx, coordinator, log); err != nil {
			log("the failsafe recovery reported a failure")
			return 1
		}
		fmt.Fprintln(stdout, "failsafe: the native UI was restored; run scripts/start.sh to restart")
		return 1
	}
	// A signalled shutdown hands the panel back but leaves the activity record
	// alone, so a reboot followed by a start resumes exclusive mode instead of
	// treating the stop as a user exit.
	if lifecycle.Exit(context.WithoutCancel(ctx)) != nil {
		log("restoring the native UI on shutdown reported a failure")
		return 1
	}
	return 0
}

// runResume is the explicit restart the failsafe waits for. It only records the
// intent; the Guardian acts on it the next time it starts.
func runResume(_ context.Context, stdout, stderr io.Writer) int {
	cfg, err := LoadConfigFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(stderr, "configuration is invalid")
		return 2
	}
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		fmt.Fprintln(stderr, "the state directory is not usable")
		return 1
	}
	if err := StoreActivity(cfg.ActivityPath, Activity{Active: true, Reason: resumeReason, At: time.Now().UTC()}); err != nil {
		fmt.Fprintln(stderr, "recording the resume request failed")
		return 1
	}
	fmt.Fprintln(stdout, "resume recorded; start the guardian to re-enter exclusive mode")
	return 0
}
