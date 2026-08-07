package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type guardianLifecycle struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (l *guardianLifecycle) Exit(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return l.err
}

func (l *guardianLifecycle) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

type timedRunner struct {
	clock     *time.Time
	durations []time.Duration
	runs      int
}

func (r *timedRunner) Run(context.Context) error {
	if r.runs < len(r.durations) {
		*r.clock = r.clock.Add(r.durations[r.runs])
	}
	r.runs++
	return errors.New("serve exited")
}

func TestSupervisorBackoffAndFailsafeAfterFiveStartFailures(t *testing.T) {
	now := time.Unix(100, 0)
	runner := &timedRunner{clock: &now, durations: []time.Duration{time.Second, time.Second, time.Second, time.Second, time.Second}}
	var sleeps []time.Duration
	supervisor := &Supervisor{
		Runner: runner,
		Now:    func() time.Time { return now },
		Sleep: func(_ context.Context, delay time.Duration) error {
			sleeps = append(sleeps, delay)
			now = now.Add(delay)
			return nil
		},
	}
	if err := supervisor.Supervise(context.Background()); !errors.Is(err, ErrFailsafe) {
		t.Fatalf("Supervise() error = %v, want ErrFailsafe", err)
	}
	if runner.runs != 5 {
		t.Fatalf("runs = %d, want 5", runner.runs)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	if !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("backoff = %v, want %v", sleeps, want)
	}
}

func TestSupervisorHealthyRunResetsFailureCount(t *testing.T) {
	now := time.Unix(200, 0)
	runner := &timedRunner{clock: &now, durations: []time.Duration{11 * time.Second, time.Second, time.Second}}
	sleeps := 0
	supervisor := &Supervisor{
		Runner: runner,
		Now:    func() time.Time { return now },
		Sleep: func(_ context.Context, delay time.Duration) error {
			now = now.Add(delay)
			sleeps++
			if sleeps == 2 {
				return context.Canceled
			}
			return nil
		},
	}
	if err := supervisor.Supervise(context.Background()); err != nil {
		t.Fatalf("Supervise() error = %v, want nil after controlled stop", err)
	}
	if runner.runs != 2 {
		t.Fatalf("runs = %d, want healthy run plus one failure", runner.runs)
	}
}

func TestSupervisorFailureWindowExpiresOldFailures(t *testing.T) {
	now := time.Unix(300, 0)
	runner := &timedRunner{clock: &now, durations: []time.Duration{time.Second, time.Second, time.Second, time.Second, time.Second, time.Second}}
	supervisor := &Supervisor{
		Runner:        runner,
		Now:           func() time.Time { return now },
		Backoff:       []time.Duration{61 * time.Second},
		FailureWindow: time.Minute,
		Sleep: func(_ context.Context, delay time.Duration) error {
			now = now.Add(delay)
			if runner.runs >= 6 {
				return context.Canceled
			}
			return nil
		},
	}
	if err := supervisor.Supervise(context.Background()); err != nil {
		t.Fatalf("Supervise() error = %v, want nil because failures aged out", err)
	}
	if runner.runs != 6 {
		t.Fatalf("runs = %d, want 6 (not failsafe)", runner.runs)
	}
}

// blockingLifecycle lets a test hold an exit in flight while concurrent
// callers pile up, so the singleflight contract is observable.
type blockingLifecycle struct {
	started chan struct{}
	release chan struct{}
	calls   int32
}

func (l *blockingLifecycle) Exit(context.Context) error {
	atomic.AddInt32(&l.calls, 1)
	close(l.started)
	<-l.release
	return nil
}

// Exit triggers can arrive simultaneously from the REST endpoint, the corner
// gesture and a repeated gesture. They must share ONE recovery execution:
// concurrent exits would double-write the activity record and run the GUI
// repaint cycle twice, interfering mid-flight.
func TestExitCoordinatorSingleflightSharesOneRecovery(t *testing.T) {
	dir := t.TempDir()
	lifecycle := &blockingLifecycle{started: make(chan struct{}), release: make(chan struct{})}
	coordinator := &ExitCoordinator{Lifecycle: lifecycle, ActivityPath: filepath.Join(dir, "activity.json")}

	first := make(chan error, 1)
	go func() { first <- coordinator.Exit(context.Background(), exitReasonGesture) }()
	<-lifecycle.started

	const waiters = 4
	results := make(chan error, waiters)
	for range waiters {
		go func() { results <- coordinator.Exit(context.Background(), exitReasonREST) }()
	}
	// Give every waiter a chance to arrive, then let the recovery finish.
	time.Sleep(50 * time.Millisecond)
	close(lifecycle.release)

	if err := <-first; err != nil {
		t.Fatalf("first exit: %v", err)
	}
	for range waiters {
		if err := <-results; err != nil {
			t.Fatalf("shared exit: %v", err)
		}
	}
	if got := atomic.LoadInt32(&lifecycle.calls); got != 1 {
		t.Fatalf("recovery executed %d times, want exactly 1 (singleflight)", got)
	}
}

// The exit-in-progress notice runs exactly once per recovery execution, while
// the device is still in exclusive mode, and never per queued caller.

func TestExitCoordinatorLeavesPendingRecoveryForRetry(t *testing.T) {
	dir := t.TempDir()
	activityPath := filepath.Join(dir, "activity.json")
	if err := StoreActivity(activityPath, Activity{Active: true, Reason: resumeReason}); err != nil {
		t.Fatal(err)
	}
	lifecycle := &guardianLifecycle{err: ErrLifecycle}
	coordinator := &ExitCoordinator{Lifecycle: lifecycle, ActivityPath: activityPath, Now: func() time.Time { return time.Unix(400, 0) }}
	if err := coordinator.Exit(context.Background(), exitReasonREST); !errors.Is(err, ErrLifecycle) {
		t.Fatalf("Exit() error = %v, want ErrLifecycle", err)
	}
	pending := LoadActivity(activityPath)
	if pending.Reason != recoveryPendingReason || !pending.Active || shouldEnterExclusive(pending) {
		t.Fatalf("failed recovery state = %+v; want non-enterable pending recovery", pending)
	}
	lifecycle.err = nil
	if err := recoverPending(context.Background(), coordinator); err != nil {
		t.Fatalf("recoverPending() error = %v", err)
	}
	if got := LoadActivity(activityPath); got.Active || got.Reason != exitReasonREST {
		t.Fatalf("recovery did not converge to inactive state: %+v", got)
	}
	if lifecycle.count() != 2 {
		t.Fatalf("lifecycle exits = %d, want retry after failure", lifecycle.count())
	}
}

func TestExitCoordinatorCompletesInactiveRecordWhenPendingWriteFails(t *testing.T) {
	dir := t.TempDir()
	activityPath := filepath.Join(dir, "activity.json")
	if err := StoreActivity(activityPath, Activity{Active: true, Reason: resumeReason}); err != nil {
		t.Fatal(err)
	}
	stores := 0
	coordinator := &ExitCoordinator{
		Lifecycle:    &guardianLifecycle{},
		ActivityPath: activityPath,
		Store: func(path string, activity Activity) error {
			stores++
			if stores == 1 {
				return errors.New("temporary activity storage failure")
			}
			return StoreActivity(path, activity)
		},
	}
	if err := coordinator.Exit(context.Background(), exitReasonREST); err != nil {
		t.Fatalf("Exit() error = %v, want completed recovery", err)
	}
	if got := LoadActivity(activityPath); got.Active || got.Reason != exitReasonREST {
		t.Fatalf("activity = %+v, want completed inactive recovery", got)
	}
}

func TestRecoverPendingPreservesFailsafeLatch(t *testing.T) {
	dir := t.TempDir()
	activityPath := filepath.Join(dir, "activity.json")
	if err := StoreActivity(activityPath, Activity{Active: true, Failsafe: true, Reason: recoveryPendingReason}); err != nil {
		t.Fatal(err)
	}
	coordinator := &ExitCoordinator{Lifecycle: &guardianLifecycle{}, ActivityPath: activityPath}
	if err := recoverPending(context.Background(), coordinator); err != nil {
		t.Fatal(err)
	}
	if got := LoadActivity(activityPath); got.Active || !got.Failsafe || got.Reason != exitReasonFailsafe {
		t.Fatalf("activity = %+v, want inactive failsafe latch", got)
	}
}

func TestActivityLatchAndResumePolicy(t *testing.T) {
	if !shouldEnterExclusive(Activity{Active: true, Reason: resumeReason}) {
		t.Fatal("explicit resume should request exclusive mode")
	}
	if shouldEnterExclusive(Activity{Active: true, Failsafe: true, Reason: resumeReason}) {
		t.Fatal("failsafe latch must suppress automatic re-entry")
	}
	if shouldEnterExclusive(Activity{Active: true, Reason: recoveryPendingReason}) {
		t.Fatal("pending recovery must run before any re-entry")
	}
}

func TestGuardianSocketExitSurvivesServiceCrashAndFailsFastWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	// macOS test worktrees can make t.TempDir paths exceed the Unix-domain
	// socket limit, so reserve a short name in /tmp for the socket itself.
	reserved, err := os.CreateTemp("/tmp", "einkrelay-guardian-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	path := reserved.Name()
	if err := reserved.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	lifecycle := &guardianLifecycle{}
	coordinator := &ExitCoordinator{Lifecycle: lifecycle, ActivityPath: filepath.Join(dir, "activity.json")}
	var exitErr error
	server := &GuardianServer{Path: path, Exit: func(ctx context.Context, reason string) error {
		exitErr = coordinator.Exit(ctx, reason)
		return exitErr
	}, Timeout: time.Second}
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer server.Close()
	go server.Serve(ctx)

	// No service process is involved: this is the path that remains usable when
	// serve has already crashed.
	client := &GuardianClient{Path: path, Timeout: time.Second}
	if err := client.ExitExclusive(context.Background()); err != nil {
		t.Fatalf("socket exit after service crash: %v (server exit: %v)", err, exitErr)
	}
	if lifecycle.count() != 1 {
		t.Fatalf("lifecycle exits = %d, want 1", lifecycle.count())
	}
	if got := LoadActivity(coordinator.ActivityPath); got.Active || got.Reason != exitReasonREST {
		t.Fatalf("socket exit did not persist deliberate inactive state: %+v", got)
	}
	if err := (&GuardianClient{Path: filepath.Join(dir, "missing.sock"), Timeout: 20 * time.Millisecond}).ExitExclusive(context.Background()); !errors.Is(err, ErrGuardian) {
		t.Fatalf("absent guardian error = %v, want ErrGuardian", err)
	}
}
