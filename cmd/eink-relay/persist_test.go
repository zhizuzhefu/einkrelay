package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type bytePanel struct {
	visible   []byte
	paths     []string
	failOn    int
	ctxs      []context.Context
	ctxErrs   []error
	deadlines []bool
}

func (p *bytePanel) show(ctx context.Context, path string) error {
	p.ctxs = append(p.ctxs, ctx)
	p.ctxErrs = append(p.ctxErrs, ctx.Err())
	_, hasDeadline := ctx.Deadline()
	p.deadlines = append(p.deadlines, hasDeadline)
	p.paths = append(p.paths, path)
	if p.failOn > 0 && len(p.paths) == p.failOn {
		return errors.New("panel failed")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	p.visible = append(p.visible[:0], payload...)
	return nil
}

type filePanel struct{ path string }

func (p filePanel) Show(_ context.Context, path string) error {
	payload, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(p.path, payload, 0600)
}

func (filePanel) Status(context.Context) BackendStatus {
	return BackendStatus{Name: "test", State: "ready"}
}

func screenPNG(t *testing.T, screen ScreenCapabilities, shade uint8) []byte {
	t.Helper()
	canvas := image.NewGray(image.Rect(0, 0, screen.Width, screen.Height))
	for index := range canvas.Pix {
		canvas.Pix[index] = shade
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, canvas); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func assertNoCandidates(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "candidate-") || entry.Name() == candidateImageName || entry.Name() == stagingImageName || strings.HasPrefix(entry.Name(), "transaction-") || strings.HasPrefix(entry.Name(), "displayed-") {
			t.Fatalf("transaction left %q behind", entry.Name())
		}
	}
}

func assertPanelMatchesCurrent(t *testing.T, panel *bytePanel, directory string, want []byte) {
	t.Helper()
	current := readFile(t, filepath.Join(directory, currentImageName))
	if !bytes.Equal(current, want) {
		t.Fatal("current.png does not contain the expected last successful screen")
	}
	if !bytes.Equal(panel.visible, current) {
		t.Fatal("the physical panel and current.png did not converge")
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestCommitPromotesOnlyAfterTheBackendSucceeds(t *testing.T) {
	screen := ScreenCapabilities{Width: 12, Height: 8}
	directory := t.TempDir()
	store := NewDisplayStore(directory)
	first := screenPNG(t, screen, 0x10)
	shown := ""
	show := func(_ context.Context, path string) error {
		shown = path
		if filepath.Base(path) == currentImageName {
			t.Error("backend was handed the committed path before promotion")
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("candidate was not durable at display time: %v", err)
		}
		if _, err := os.Stat(filepath.Join(directory, currentImageName)); err == nil {
			t.Error("current.png existed before the first commit finished")
		}
		return nil
	}
	result, err := store.Commit(context.Background(), first, screen, show)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.SHA256) != 64 || result.DisplayedAt.IsZero() || shown == "" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if !bytes.Equal(readFile(t, filepath.Join(directory, currentImageName)), first) {
		t.Fatal("current.png does not hold the committed payload")
	}
	assertNoCandidates(t, directory)

	second := screenPNG(t, screen, 0xa0)
	if _, err := store.Commit(context.Background(), second, screen, func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, filepath.Join(directory, currentImageName)), second) {
		t.Fatal("the second commit did not become current")
	}
	if !bytes.Equal(readFile(t, filepath.Join(directory, previousImageName)), first) {
		t.Fatal("the superseded screen was not rotated into previous.png")
	}
	assertNoCandidates(t, directory)
}

func TestInterruptionAtAnyStepKeepsTheLastSuccessfulScreen(t *testing.T) {
	screen := ScreenCapabilities{Width: 12, Height: 8}
	good := screenPNG(t, screen, 0x10)
	candidate := screenPNG(t, screen, 0xf0)
	interrupted := errors.New("interrupted")
	cases := []struct {
		name    string
		arrange func(*DisplayStore, *func(context.Context, string) error)
	}{
		{
			name: "candidate written",
			arrange: func(store *DisplayStore, _ *func(context.Context, string) error) {
				store.hooks.afterCandidate = func() error { return interrupted }
			},
		},
		{
			name: "backend failed",
			arrange: func(_ *DisplayStore, show *func(context.Context, string) error) {
				*show = func(context.Context, string) error { return interrupted }
			},
		},
		{
			name: "after display",
			arrange: func(store *DisplayStore, _ *func(context.Context, string) error) {
				store.hooks.afterDisplay = func() error { return interrupted }
			},
		},
		{
			name: "rotation",
			arrange: func(store *DisplayStore, _ *func(context.Context, string) error) {
				failed := false
				store.hooks.renameFile = func(oldPath, newPath string) error {
					if !failed && filepath.Base(oldPath) == stagingImageName && filepath.Base(newPath) == previousImageName {
						failed = true
						return interrupted
					}
					return os.Rename(oldPath, newPath)
				}
			},
		},
		{
			name: "after rotate",
			arrange: func(store *DisplayStore, _ *func(context.Context, string) error) {
				store.hooks.afterRotate = func() error { return interrupted }
			},
		},
		{
			name: "candidate promotion",
			arrange: func(store *DisplayStore, _ *func(context.Context, string) error) {
				failed := false
				store.hooks.renameFile = func(oldPath, newPath string) error {
					if !failed && filepath.Base(oldPath) == candidateImageName && filepath.Base(newPath) == currentImageName {
						failed = true
						return interrupted
					}
					return os.Rename(oldPath, newPath)
				}
			},
		},
		{
			name: "directory fsync",
			arrange: func(store *DisplayStore, _ *func(context.Context, string) error) {
				failed := false
				store.hooks.syncDirectory = func(path string) error {
					if !failed {
						failed = true
						return interrupted
					}
					return syncDir(path)
				}
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			store := NewDisplayStore(directory)
			if _, err := store.Commit(context.Background(), good, screen, func(context.Context, string) error { return nil }); err != nil {
				t.Fatal(err)
			}
			panel := &bytePanel{visible: append([]byte(nil), good...)}
			show := panel.show
			test.arrange(store, &show)
			if _, err := store.Commit(context.Background(), candidate, screen, show); !errors.Is(err, interrupted) {
				t.Fatalf("interruption was swallowed: %v", err)
			}
			assertPanelMatchesCurrent(t, panel, directory, good)
			current := readFile(t, filepath.Join(directory, currentImageName))
			if err := validateFullScreenPNG(current, screen); err != nil {
				t.Fatalf("current.png is no longer verifiable: %v", err)
			}
			assertNoCandidates(t, directory)
			recovered, err := store.Recover(screen)
			if err != nil || !bytes.Equal(recovered.Payload, good) {
				t.Fatalf("recovery did not return the last successful screen: %v", err)
			}
		})
	}
}

func TestFirstDisplayFailureCommitsForward(t *testing.T) {
	screen := ScreenCapabilities{Width: 12, Height: 8}
	directory := t.TempDir()
	store := NewDisplayStore(directory)
	panel := &bytePanel{}
	store.hooks.afterDisplay = func() error { return errors.New("interrupted") }
	payload := screenPNG(t, screen, 0x90)
	result, err := store.Commit(context.Background(), payload, screen, panel.show)
	if err != nil {
		t.Fatalf("first display could not be rolled back and should have committed forward: %v", err)
	}
	if len(result.SHA256) != 64 || result.DisplayedAt.IsZero() {
		t.Fatalf("forward commit returned an incomplete result: %+v", result)
	}
	assertPanelMatchesCurrent(t, panel, directory, payload)
	assertNoCandidates(t, directory)
}

func TestFailedTransactionRestoresTwoGenerationHistory(t *testing.T) {
	screen := ScreenCapabilities{Width: 12, Height: 8}
	directory := t.TempDir()
	store := NewDisplayStore(directory)
	panel := &bytePanel{}
	previous := screenPNG(t, screen, 0x10)
	current := screenPNG(t, screen, 0x80)
	candidate := screenPNG(t, screen, 0xf0)
	for _, payload := range [][]byte{previous, current} {
		if _, err := store.Commit(context.Background(), payload, screen, panel.show); err != nil {
			t.Fatal(err)
		}
	}
	store.hooks.afterRotate = func() error { return errors.New("stop after rotate") }
	if _, err := store.Commit(context.Background(), candidate, screen, panel.show); err == nil {
		t.Fatal("interrupted commit succeeded")
	}
	assertPanelMatchesCurrent(t, panel, directory, current)
	if got := readFile(t, filepath.Join(directory, previousImageName)); !bytes.Equal(got, previous) {
		t.Fatal("a failed transaction destroyed the previous generation")
	}
	assertNoCandidates(t, directory)
}

func TestLinkUnsupportedFallbackNeverMovesCurrentAway(t *testing.T) {
	screen := ScreenCapabilities{Width: 12, Height: 8}
	directory := t.TempDir()
	store := NewDisplayStore(directory)
	panel := &bytePanel{}
	old := screenPNG(t, screen, 0x20)
	if _, err := store.Commit(context.Background(), old, screen, panel.show); err != nil {
		t.Fatal(err)
	}
	fallbackHit := false
	store.hooks.linkFile = func(_ string, newPath string) error {
		if filepath.Base(newPath) == stagingImageName {
			fallbackHit = true
		}
		return syscall.EOPNOTSUPP
	}
	store.hooks.afterRotate = func() error {
		if got := readFile(t, filepath.Join(directory, currentImageName)); !bytes.Equal(got, old) {
			t.Fatal("link fallback moved current away")
		}
		if got := readFile(t, filepath.Join(directory, previousImageName)); !bytes.Equal(got, old) {
			t.Fatal("link fallback did not publish a copied previous")
		}
		return errors.New("stop")
	}
	if _, err := store.Commit(context.Background(), screenPNG(t, screen, 0xe0), screen, panel.show); err == nil {
		t.Fatal("interrupted fallback commit succeeded")
	}
	if !fallbackHit {
		t.Fatal("test did not execute the link-unsupported fallback")
	}
	assertPanelMatchesCurrent(t, panel, directory, old)
}

func TestRollbackUsesValidPreviousWhenCurrentIsCorrupt(t *testing.T) {
	screen := ScreenCapabilities{Width: 12, Height: 8}
	directory := t.TempDir()
	store := NewDisplayStore(directory)
	panel := &bytePanel{}
	previous := screenPNG(t, screen, 0x30)
	current := screenPNG(t, screen, 0x70)
	for _, payload := range [][]byte{previous, current} {
		if _, err := store.Commit(context.Background(), payload, screen, panel.show); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, currentImageName), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreLastScreen(context.Background(), store, filePanel{path: filepath.Join(directory, "panel.bin")}, screen); err != nil {
		t.Fatal(err)
	}
	panel.visible = append(panel.visible[:0], previous...)
	store.hooks.afterDisplay = func() error { return errors.New("stop") }
	if _, err := store.Commit(context.Background(), screenPNG(t, screen, 0xe0), screen, panel.show); err == nil {
		t.Fatal("interrupted commit succeeded")
	}
	assertPanelMatchesCurrent(t, panel, directory, previous)
}

func TestRecoverNeverPromotesCandidateWithoutDisplayedPhase(t *testing.T) {
	screen := ScreenCapabilities{Width: 12, Height: 8}
	directory := t.TempDir()
	store := NewDisplayStore(directory)
	payload := screenPNG(t, screen, 0x90)
	if _, err := store.writeCandidate(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Recover(screen); !errors.Is(err, ErrPersistence) {
		t.Fatalf("an unshown candidate was accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, currentImageName)); !os.IsNotExist(err) {
		t.Fatal("an unshown candidate was promoted to current")
	}
	panel := &bytePanel{}
	next := screenPNG(t, screen, 0x50)
	if _, err := store.Commit(context.Background(), next, screen, panel.show); err != nil {
		t.Fatalf("an incomplete first transaction wedged later commits: %v", err)
	}
	assertPanelMatchesCurrent(t, panel, directory, next)
}

func TestFirstPreparedTransactionDoesNotWedgeRecovery(t *testing.T) {
	screen := ScreenCapabilities{Width: 12, Height: 8}
	directory := t.TempDir()
	store := NewDisplayStore(directory)
	if _, err := store.writeCandidate(screenPNG(t, screen, 0x90)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.prepareRollback(screen); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Recover(screen); !errors.Is(err, ErrPersistence) {
		t.Fatalf("a merely prepared first candidate was accepted: %v", err)
	}
	panel := &bytePanel{}
	next := screenPNG(t, screen, 0x50)
	if _, err := store.Commit(context.Background(), next, screen, panel.show); err != nil {
		t.Fatalf("prepared first transaction wedged later commits: %v", err)
	}
	assertPanelMatchesCurrent(t, panel, directory, next)
}

func TestRollbackUsesBoundedContextIndependentOfRequest(t *testing.T) {
	screen := ScreenCapabilities{Width: 12, Height: 8}
	directory := t.TempDir()
	store := NewDisplayStore(directory)
	panel := &bytePanel{}
	old := screenPNG(t, screen, 0x20)
	if _, err := store.Commit(context.Background(), old, screen, panel.show); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	store.hooks.afterDisplay = func() error {
		cancel()
		return errors.New("stop")
	}
	if _, err := store.Commit(ctx, screenPNG(t, screen, 0xe0), screen, panel.show); err == nil {
		t.Fatal("interrupted commit succeeded")
	}
	if len(panel.ctxs) < 3 {
		t.Fatalf("expected a compensating display call, got %d calls", len(panel.ctxs))
	}
	index := len(panel.ctxs) - 1
	if panel.ctxErrs[index] != nil {
		t.Fatalf("compensation inherited request cancellation: %v", panel.ctxErrs[index])
	}
	if !panel.deadlines[index] {
		t.Fatal("compensation context has no deadline")
	}
	assertPanelMatchesCurrent(t, panel, directory, old)
}

func TestRollbackFailurePreservesDisplayedCandidate(t *testing.T) {
	screen := ScreenCapabilities{Width: 12, Height: 8}
	directory := t.TempDir()
	store := NewDisplayStore(directory)
	panel := &bytePanel{}
	old := screenPNG(t, screen, 0x20)
	if _, err := store.Commit(context.Background(), old, screen, panel.show); err != nil {
		t.Fatal(err)
	}
	panel.failOn = len(panel.paths) + 2
	store.hooks.afterDisplay = func() error { return errors.New("stop") }
	newScreen := screenPNG(t, screen, 0xe0)
	if _, err := store.Commit(context.Background(), newScreen, screen, panel.show); err == nil {
		t.Fatal("commit succeeded despite failed rollback display")
	}
	if !bytes.Equal(panel.visible, newScreen) {
		t.Fatal("test panel did not retain the newly displayed bytes")
	}
	foundPending := false
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), pendingDisplayPrefix) && bytes.Equal(readFile(t, filepath.Join(directory, entry.Name())), newScreen) {
			foundPending = true
		}
	}
	if !foundPending {
		t.Fatal("the durable pending copy of the displayed candidate was deleted")
	}

	// A later request that fails before changing the panel must not overwrite
	// or remove the only durable copy of the bytes that remain visible.
	store.hooks = transactionHooks{}
	panel.failOn = len(panel.paths) + 1
	if _, err := store.Commit(context.Background(), screenPNG(t, screen, 0x40), screen, panel.show); err == nil {
		t.Fatal("backend failure was accepted")
	}
	if !bytes.Equal(panel.visible, newScreen) {
		t.Fatal("a later failed request changed the panel")
	}
	foundPending = false
	entries, err = os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), pendingDisplayPrefix) && bytes.Equal(readFile(t, filepath.Join(directory, entry.Name())), newScreen) {
			foundPending = true
		}
	}
	if !foundPending {
		t.Fatal("a later failed request removed the visible screen's pending copy")
	}
}

func TestCommitRejectsPayloadsThatAreNotFullScreenGrayscale(t *testing.T) {
	screen := ScreenCapabilities{Width: 12, Height: 8}
	directory := t.TempDir()
	store := NewDisplayStore(directory)
	calls := 0
	show := func(context.Context, string) error {
		calls++
		return nil
	}
	var colour bytes.Buffer
	if err := png.Encode(&colour, image.NewRGBA(image.Rect(0, 0, screen.Width, screen.Height))); err != nil {
		t.Fatal(err)
	}
	cases := [][]byte{
		screenPNG(t, ScreenCapabilities{Width: 11, Height: 8}, 0x40),
		colour.Bytes(),
		[]byte("not a png"),
	}
	for index, payload := range cases {
		if _, err := store.Commit(context.Background(), payload, screen, show); !errors.Is(err, ErrDecodeFailed) {
			t.Fatalf("case %d was accepted: %v", index, err)
		}
	}
	if calls != 0 {
		t.Fatal("an invalid payload reached the display backend")
	}
	if _, err := os.Stat(filepath.Join(directory, currentImageName)); err == nil {
		t.Fatal("an invalid payload was committed")
	}
	assertNoCandidates(t, directory)
}

func TestRecoverFallsBackToPreviousAndRefusesUnvalidatedData(t *testing.T) {
	screen := ScreenCapabilities{Width: 12, Height: 8}
	directory := t.TempDir()
	store := NewDisplayStore(directory)
	first := screenPNG(t, screen, 0x10)
	second := screenPNG(t, screen, 0xa0)
	for _, payload := range [][]byte{first, second} {
		if _, err := store.Commit(context.Background(), payload, screen, func(context.Context, string) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	recovered, err := store.Recover(screen)
	if err != nil || recovered.Name != currentImageName || !bytes.Equal(recovered.Payload, second) {
		t.Fatalf("recovery did not prefer current.png: %+v %v", recovered.Name, err)
	}
	if len(recovered.SHA256) != 64 {
		t.Fatalf("recovery did not report a digest: %q", recovered.SHA256)
	}
	// Truncating current.png simulates a crash between the write and the
	// durable rename; previous.png must take over.
	if err := os.WriteFile(filepath.Join(directory, currentImageName), second[:len(second)/2], 0600); err != nil {
		t.Fatal(err)
	}
	fallback, err := store.Recover(screen)
	if err != nil || fallback.Name != previousImageName || !bytes.Equal(fallback.Payload, first) {
		t.Fatalf("corrupt current.png did not fall back to previous.png: %+v %v", fallback.Name, err)
	}
	if err := os.WriteFile(filepath.Join(directory, previousImageName), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Recover(screen); !errors.Is(err, ErrPersistence) {
		t.Fatalf("recovery returned unvalidated data: %v", err)
	}
}

func TestDurablePhaseCrashWithCorruptCurrentFallsBackToPrevious(t *testing.T) {
	screen := ScreenCapabilities{Width: 12, Height: 8}
	directory := t.TempDir()
	store := NewDisplayStore(directory)
	first := screenPNG(t, screen, 0x10)
	second := screenPNG(t, screen, 0xa0)
	for _, payload := range [][]byte{first, second} {
		if _, err := store.Commit(context.Background(), payload, screen, func(context.Context, string) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	// A crash between the durable phase marker and its cleanup leaves the marker
	// on disk. current.png is then the promoted screen, but if its bytes did not
	// survive the interruption the rotated previous generation is still verified
	// and must be restored instead of refusing to restore anything.
	if err := store.writeTransactionPhase(phaseDurable); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, currentImageName), second[:len(second)/2], 0600); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Recover(screen)
	if err != nil || recovered.Name != previousImageName || !bytes.Equal(recovered.Payload, first) {
		t.Fatalf("a durable-phase crash with a corrupt current did not fall back to previous.png: %q %v", recovered.Name, err)
	}
	assertNoCandidates(t, directory)
	again, err := store.Recover(screen)
	if err != nil || !bytes.Equal(again.Payload, first) {
		t.Fatalf("the second recovery was not idempotent: %v", err)
	}
}

func TestPersistCrashHelper(t *testing.T) {
	if os.Getenv("EINKRELAY_PERSIST_HELPER") != "1" {
		return
	}
	directory := os.Getenv("EINKRELAY_PERSIST_DIR")
	phase := os.Getenv("EINKRELAY_PERSIST_PHASE")
	store := NewDisplayStore(directory)
	panel := filePanel{path: filepath.Join(directory, "panel.bin")}
	crash := func() error {
		os.Exit(86)
		return nil
	}
	switch phase {
	case "after-candidate":
		store.hooks.afterCandidate = crash
	case "snapshot-midpoint":
		store.hooks.linkFile = func(oldPath, newPath string) error {
			if filepath.Base(newPath) == rollbackPreviousName {
				return crash()
			}
			return os.Link(oldPath, newPath)
		}
	case "phase-update":
		updates := 0
		store.hooks.renameFile = func(oldPath, newPath string) error {
			if filepath.Base(newPath) == transactionPhaseName {
				updates++
				if updates == 2 {
					return crash()
				}
			}
			return os.Rename(oldPath, newPath)
		}
	case "after-display":
		store.hooks.afterDisplay = crash
	case "first-after-display":
		store.hooks.afterDisplay = crash
	case "after-rotate":
		store.hooks.linkFile = func(string, string) error { return syscall.EOPNOTSUPP }
		store.hooks.afterRotate = crash
	case "after-promote":
		store.hooks.afterPromote = crash
	default:
		t.Fatalf("unknown crash phase %q", phase)
	}
	screen := ScreenCapabilities{Width: 12, Height: 8}
	_, _ = store.Commit(context.Background(), screenPNG(t, screen, 0xf0), screen, panel.Show)
	t.Fatal("helper did not crash")
}

func TestHelperProcessCrashRecoveryConvergesPanelAndHistory(t *testing.T) {
	screen := ScreenCapabilities{Width: 12, Height: 8}
	for _, test := range []struct {
		phase        string
		wantShade    uint8
		wantPrevious uint8
		firstDisplay bool
	}{
		{phase: "after-candidate", wantShade: 0x80, wantPrevious: 0x10},
		{phase: "snapshot-midpoint", wantShade: 0x80, wantPrevious: 0x10},
		{phase: "phase-update", wantShade: 0x80, wantPrevious: 0x10},
		{phase: "after-display", wantShade: 0x80, wantPrevious: 0x10},
		{phase: "after-rotate", wantShade: 0x80, wantPrevious: 0x10},
		{phase: "after-promote", wantShade: 0x80, wantPrevious: 0x10},
		{phase: "first-after-display", wantShade: 0xf0, firstDisplay: true},
	} {
		t.Run(test.phase, func(t *testing.T) {
			directory := t.TempDir()
			store := NewDisplayStore(directory)
			panelPath := filepath.Join(directory, "panel.bin")
			panel := filePanel{path: panelPath}
			if !test.firstDisplay {
				for _, shade := range []uint8{0x10, 0x80} {
					if _, err := store.Commit(context.Background(), screenPNG(t, screen, shade), screen, panel.Show); err != nil {
						t.Fatal(err)
					}
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPersistCrashHelper$")
			command.Env = append(os.Environ(),
				"EINKRELAY_PERSIST_HELPER=1",
				"EINKRELAY_PERSIST_DIR="+directory,
				"EINKRELAY_PERSIST_PHASE="+test.phase,
			)
			err := command.Run()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != 86 {
				t.Fatalf("helper did not exit at the crash boundary: %v", err)
			}
			if ctx.Err() != nil {
				t.Fatalf("helper timed out: %v", ctx.Err())
			}
			candidate := screenPNG(t, screen, 0xf0)
			wantBeforeRecovery := candidate
			if test.phase == "after-candidate" || test.phase == "snapshot-midpoint" {
				wantBeforeRecovery = screenPNG(t, screen, 0x80)
			}
			if got := readFile(t, panelPath); !bytes.Equal(got, wantBeforeRecovery) {
				t.Fatal("helper reached an unexpected panel state before crashing")
			}

			restarted := NewDisplayStore(directory)
			recovered, err := RestoreLastScreen(context.Background(), restarted, panel, screen)
			if err != nil {
				t.Fatal(err)
			}
			want := screenPNG(t, screen, test.wantShade)
			if !bytes.Equal(recovered.Payload, want) || !bytes.Equal(readFile(t, panelPath), want) || !bytes.Equal(readFile(t, filepath.Join(directory, currentImageName)), want) {
				t.Fatal("restart did not converge the panel and current image")
			}
			var wantPrevious []byte
			if test.firstDisplay {
				if _, err := os.Stat(filepath.Join(directory, previousImageName)); !os.IsNotExist(err) {
					t.Fatal("first display recovery invented a previous generation")
				}
			} else {
				wantPrevious = screenPNG(t, screen, test.wantPrevious)
				if !bytes.Equal(readFile(t, filepath.Join(directory, previousImageName)), wantPrevious) {
					t.Fatal("crash recovery lost the previous generation")
				}
			}
			second, err := RestoreLastScreen(context.Background(), restarted, panel, screen)
			if err != nil {
				t.Fatalf("second recovery was not idempotent: %v", err)
			}
			previousUnchanged := test.firstDisplay
			if !test.firstDisplay {
				previousUnchanged = bytes.Equal(readFile(t, filepath.Join(directory, previousImageName)), wantPrevious)
			}
			if !bytes.Equal(second.Payload, want) || !bytes.Equal(readFile(t, panelPath), want) || !bytes.Equal(readFile(t, filepath.Join(directory, currentImageName)), want) || !previousUnchanged {
				t.Fatal("second recovery changed the recovered state")
			}
		})
	}
}

// TestVerifyStoredFrameCatchesEveryCorruptionTheDecodeDid is the safety case
// for not decoding stored frames any more. The cheap check has to reject
// everything the full decode rejected, or a corrupt rollback baseline could be
// chosen and then displayed.
func TestVerifyStoredFrameCatchesEveryCorruptionTheDecodeDid(t *testing.T) {
	screen := ScreenCapabilities{Width: 40, Height: 25}
	good := grayFixture(t, screen.Width, screen.Height, func(x, y int) uint8 { return uint8(x*7 + y*3) })
	if err := verifyStoredFullScreenPNG(good, screen); err != nil {
		t.Fatalf("a frame this store wrote was rejected: %v", err)
	}

	corruptions := map[string][]byte{
		"empty":              {},
		"signature only":     append([]byte(nil), good[:8]...),
		"truncated mid file": append([]byte(nil), good[:len(good)/2]...),
		"missing IEND":       append([]byte(nil), good[:len(good)-12]...),
		"trailing garbage":   append(append([]byte(nil), good...), 0x00),
	}
	// A single flipped bit anywhere in the payload must be caught by a chunk CRC.
	for _, offset := range []int{20, len(good) / 3, len(good) / 2, len(good) - 20} {
		flipped := append([]byte(nil), good...)
		flipped[offset] ^= 0x01
		corruptions[fmt.Sprintf("bit flip at %d", offset)] = flipped
	}
	for name, payload := range corruptions {
		if err := verifyStoredFullScreenPNG(payload, screen); err == nil {
			t.Fatalf("%s was accepted as an intact stored frame", name)
		}
	}

	// A frame written for a different panel is not a frame for this one. This
	// is the case a changed geometry probe produces, and it has to be refused
	// rather than displayed at the wrong size.
	other := grayFixture(t, screen.Width+1, screen.Height, func(x, y int) uint8 { return 0 })
	if err := verifyStoredFullScreenPNG(other, screen); err == nil {
		t.Fatal("a frame with the wrong geometry was accepted")
	}
}

// TestVerifyStoredFrameAgreesWithTheDecoder pins the two checks to the same
// verdict on the inputs the store can actually produce, so the cheap one can
// stand in for the expensive one.
func TestVerifyStoredFrameAgreesWithTheDecoder(t *testing.T) {
	screen := ScreenCapabilities{Width: 24, Height: 16}
	for _, shade := range []func(x, y int) uint8{
		func(int, int) uint8 { return 0x00 },
		func(int, int) uint8 { return 0xff },
		func(x, y int) uint8 { return uint8(x ^ y) },
	} {
		payload := grayFixture(t, screen.Width, screen.Height, shade)
		deep := validateFullScreenPNG(payload, screen)
		cheap := verifyStoredFullScreenPNG(payload, screen)
		if (deep == nil) != (cheap == nil) {
			t.Fatalf("verdicts disagree: decode=%v structural=%v", deep, cheap)
		}
	}
}

func BenchmarkValidateStoredFrameByDecoding(b *testing.B) {
	screen := benchScreen
	canvas := image.NewGray(image.Rect(0, 0, screen.Width, screen.Height))
	for index := range canvas.Pix {
		canvas.Pix[index] = uint8(index % 251)
	}
	var buffer bytes.Buffer
	if err := encodeScreenPNG(&buffer, canvas); err != nil {
		b.Fatal(err)
	}
	payload := buffer.Bytes()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := validateFullScreenPNG(payload, screen); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidateStoredFrameStructurally(b *testing.B) {
	screen := benchScreen
	canvas := image.NewGray(image.Rect(0, 0, screen.Width, screen.Height))
	for index := range canvas.Pix {
		canvas.Pix[index] = uint8(index % 251)
	}
	var buffer bytes.Buffer
	if err := encodeScreenPNG(&buffer, canvas); err != nil {
		b.Fatal(err)
	}
	payload := buffer.Bytes()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := verifyStoredFullScreenPNG(payload, screen); err != nil {
			b.Fatal(err)
		}
	}
}

// TestVerifyStoredFrameSurvivesAnAbsurdChunkLength is an arithmetic guard, not
// a format one. The chunk length is a uint32 and the target is 32-bit ARM,
// where an int is 32 bits too: adding a length close to 2GiB to the offset
// wraps negative, a naive bounds test passes, and the slice that follows
// panics. A corrupt frame sitting in the state directory would then take the
// whole service down on the next transaction.
func TestVerifyStoredFrameSurvivesAnAbsurdChunkLength(t *testing.T) {
	screen := ScreenCapabilities{Width: 16, Height: 8}
	good := grayFixture(t, screen.Width, screen.Height, func(int, int) uint8 { return 0x40 })
	for _, declared := range []uint32{0x7fffffff, 0x7ffffff0, 0x80000000, 0xffffffff, 0xfffffff0} {
		payload := append([]byte(nil), good...)
		// Overwrite the length of the first chunk after the signature.
		binary.BigEndian.PutUint32(payload[8:12], declared)
		if err := verifyStoredFullScreenPNG(payload, screen); err == nil {
			t.Fatalf("a chunk declaring %#x bytes was accepted", declared)
		}
	}
	// The same shape reached through the store's own entry point.
	store := &DisplayStore{Dir: t.TempDir()}
	poisoned := append([]byte(nil), good...)
	binary.BigEndian.PutUint32(poisoned[8:12], 0xfffffff0)
	path := filepath.Join(store.Dir, currentImageName)
	if err := os.WriteFile(path, poisoned, 0600); err != nil {
		t.Fatal(err)
	}
	if store.validRegularPNG(path, screen) {
		t.Fatal("the store accepted a frame with an absurd chunk length")
	}
}

// TestPendingDisplaysNeverAccumulate covers a slow disk leak on a narrow but
// real failure path: the panel accepts a frame, a later durable step fails, and
// the rollback re-display fails too. That combination preserves a uniquely
// named copy of the frame that may still be visible and then returns without
// reaching any cleanup, so each occurrence left roughly 200KB behind on the
// small root partition that also holds the token and the activity record.
//
// Note which scenario does NOT leak: a panel that rejects the very first show
// short-circuits before any copy is made. That is why this went unnoticed — the
// obvious "the display is broken" case is not the one that accumulates.
func TestPendingDisplaysNeverAccumulate(t *testing.T) {
	directory := t.TempDir()
	screen := ScreenCapabilities{Width: 32, Height: 24}
	store := NewDisplayStore(directory)
	payload := grayFixture(t, screen.Width, screen.Height, func(int, int) uint8 { return 0x20 })

	// One good frame first, so later transactions have a rollback baseline and
	// take the branch that preserves a pending copy.
	if _, err := store.Commit(context.Background(), payload, screen, func(context.Context, string) error { return nil }); err != nil {
		t.Fatalf("seeding the store failed: %v", err)
	}

	// The panel accepts the candidate, a durable step then fails, and the
	// rollback re-display fails as well.
	store.hooks.afterDisplay = func() error { return errors.New("the state directory went away") }
	shows := 0
	flaky := func(context.Context, string) error {
		shows++
		if shows == 1 {
			return nil // the candidate reaches the panel
		}
		return errors.New("the panel stopped answering") // the rollback does not
	}

	for attempt := 0; attempt < 25; attempt++ {
		shows = 0
		frame := grayFixture(t, screen.Width, screen.Height, func(x, y int) uint8 { return uint8(attempt) })
		if _, err := store.Commit(context.Background(), frame, screen, flaky); err == nil {
			t.Fatal("a failed transaction reported success")
		}
		if pending := countPendingDisplays(t, directory); pending > 1 {
			t.Fatalf("after %d failures the store held %d pending copies; at most one is ever meaningful", attempt+1, pending)
		}
	}

	// The newest one is still there: it is the frame that may be on the panel.
	if pending := countPendingDisplays(t, directory); pending != 1 {
		t.Fatalf("the newest pending copy was not retained: %d", pending)
	}

	// A transaction that succeeds clears it.
	store.hooks.afterDisplay = nil
	if _, err := store.Commit(context.Background(), payload, screen, func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if pending := countPendingDisplays(t, directory); pending != 0 {
		t.Fatalf("a successful transaction left %d pending copies behind", pending)
	}
}

func countPendingDisplays(t *testing.T, directory string) int {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), pendingDisplayPrefix) {
			count++
		}
	}
	return count
}
