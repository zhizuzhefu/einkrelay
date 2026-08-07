package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ErrPersistence maps to the frozen 500 persistence_failed response.
var ErrPersistence = errors.New("persistence failed")

const (
	currentImageName       = "current.png"
	previousImageName      = "previous.png"
	candidateImageName     = "candidate.png"
	stagingImageName       = "previous-staging.png"
	rollbackCurrentName    = "transaction-current.png"
	rollbackPreviousName   = "transaction-previous.png"
	transactionPhaseName   = "transaction-phase"
	pendingDisplayPrefix   = "displayed-pending-"
	rollbackDisplayTimeout = 15 * time.Second
)

const (
	phasePrepared  = "prepared"
	phaseDisplayed = "displayed"
	phaseRotated   = "rotated"
	phasePromoted  = "promoted"
	phaseDurable   = "durable"
)

// DisplayStore owns the durable half of a display transaction. The order is
// fixed: write a candidate, validate it, fsync it, hand it to the backend, and
// only once the backend has succeeded rotate current into previous and promote
// the candidate. Nothing is cleared up front and nothing is promoted early, so
// an interruption at any step leaves the last successful screen intact.
type DisplayStore struct {
	Dir string
	// hooks stay nil on the production path. Tests set them to interrupt the
	// transaction at a specific step.
	hooks transactionHooks
}

type transactionHooks struct {
	afterCandidate func() error
	afterDisplay   func() error
	afterRotate    func() error
	afterPromote   func() error
	linkFile       func(string, string) error
	renameFile     func(string, string) error
	syncDirectory  func(string) error
}

type rollbackState struct {
	currentExisted  bool
	previousExisted bool
	baseline        string
}

func NewDisplayStore(dir string) *DisplayStore { return &DisplayStore{Dir: dir} }

func (s *DisplayStore) path(name string) string { return filepath.Join(s.Dir, name) }

// Commit runs the display transaction. show receives the path of the validated
// candidate, never the committed path, so the backend can only ever be asked to
// display bytes that have already survived validation and fsync.
func (s *DisplayStore) Commit(ctx context.Context, payload []byte, screen ScreenCapabilities, show func(context.Context, string) error) (DisplayResult, error) {
	if err := validateFullScreenPNG(payload, screen); err != nil {
		return DisplayResult{}, err
	}
	candidate, err := s.writeCandidate(payload)
	if err != nil {
		return DisplayResult{}, err
	}
	removeCandidate := true
	defer func() {
		if removeCandidate {
			_ = os.Remove(candidate)
		}
	}()
	if err := runHook(s.hooks.afterCandidate); err != nil {
		return DisplayResult{}, err
	}
	rollback, err := s.prepareRollback(screen)
	if err != nil {
		return DisplayResult{}, err
	}
	if err := show(ctx, candidate); err != nil {
		s.cleanupActiveTransaction()
		return DisplayResult{}, err
	}
	// From this point the panel contains candidate. It may only be removed once
	// the canonical files and the panel have converged again.
	removeCandidate = false
	if err := s.writeTransactionPhase(phaseDisplayed); err != nil {
		return s.reconcilePostDisplay(payload, screen, candidate, rollback, show, err)
	}
	if err := runHook(s.hooks.afterDisplay); err != nil {
		return s.reconcilePostDisplay(payload, screen, candidate, rollback, show, err)
	}
	if err := s.rotate(rollback.baseline); err != nil {
		return s.reconcilePostDisplay(payload, screen, candidate, rollback, show, err)
	}
	if err := s.writeTransactionPhase(phaseRotated); err != nil {
		return s.reconcilePostDisplay(payload, screen, candidate, rollback, show, err)
	}
	if err := runHook(s.hooks.afterRotate); err != nil {
		return s.reconcilePostDisplay(payload, screen, candidate, rollback, show, err)
	}
	if err := s.rename(candidate, s.path(currentImageName)); err != nil {
		return s.reconcilePostDisplay(payload, screen, candidate, rollback, show, errors.Join(ErrPersistence, err))
	}
	if err := s.writeTransactionPhase(phasePromoted); err != nil {
		return s.reconcilePostDisplay(payload, screen, candidate, rollback, show, err)
	}
	if err := runHook(s.hooks.afterPromote); err != nil {
		return s.reconcilePostDisplay(payload, screen, candidate, rollback, show, err)
	}
	// A rename is a directory metadata change. Without the directory fsync the
	// entry can be lost on power removal, which is how a Kindle normally shuts
	// down.
	if err := s.syncDirectory(); err != nil {
		return s.reconcilePostDisplay(payload, screen, candidate, rollback, show, errors.Join(ErrPersistence, err))
	}
	if err := s.writeTransactionPhase(phaseDurable); err != nil {
		return s.reconcilePostDisplay(payload, screen, candidate, rollback, show, err)
	}
	s.cleanupActiveTransaction()
	s.cleanupPendingDisplays("")
	digest := sha256.Sum256(payload)
	return DisplayResult{SHA256: hex.EncodeToString(digest[:]), DisplayedAt: time.Now().UTC()}, nil
}

func (s *DisplayStore) writeCandidate(payload []byte) (string, error) {
	name := s.path(candidateImageName)
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", ErrPersistence
	}
	abandon := func() (string, error) {
		_ = file.Close()
		_ = os.Remove(name)
		return "", ErrPersistence
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
	// A close error can still report a deferred write failure, so it is
	// checked rather than discarded.
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return "", ErrPersistence
	}
	return name, nil
}

// rotate publishes the verified rollback baseline as previous. Linking first
// keeps current visible for the whole operation. Filesystems without hard-link
// support copy and fsync staging instead; current is never renamed away.
func (s *DisplayStore) rotate(baseline string) error {
	if baseline == "" {
		return nil
	}
	info, err := os.Lstat(baseline)
	if err != nil {
		return ErrPersistence
	}
	if !info.Mode().IsRegular() {
		return ErrPersistence
	}
	staging := s.path(stagingImageName)
	_ = os.Remove(staging)
	if err := s.link(baseline, staging); err != nil {
		if !linkUnsupported(err) {
			return errors.Join(ErrPersistence, err)
		}
		if err := copyDurableFile(baseline, staging); err != nil {
			return ErrPersistence
		}
	}
	if err := s.rename(staging, s.path(previousImageName)); err != nil {
		_ = os.Remove(staging)
		return errors.Join(ErrPersistence, err)
	}
	return nil
}

// prepareRollback snapshots both canonical generations before FBInk runs. The
// snapshots are same-directory durable names, so a failure after rotation can
// restore both current and previous instead of silently consuming one level of
// history.
func (s *DisplayStore) prepareRollback(screen ScreenCapabilities) (rollbackState, error) {
	state := rollbackState{}
	_ = os.Remove(s.path(rollbackCurrentName))
	_ = os.Remove(s.path(rollbackPreviousName))
	_ = os.Remove(s.path(transactionPhaseName))

	var err error
	state.currentExisted, err = s.snapshotCanonical(currentImageName, rollbackCurrentName)
	if err != nil {
		return rollbackState{}, err
	}
	state.previousExisted, err = s.snapshotCanonical(previousImageName, rollbackPreviousName)
	if err != nil {
		return rollbackState{}, err
	}
	if state.currentExisted || state.previousExisted {
		if err := syncDir(s.Dir); err != nil {
			return rollbackState{}, ErrPersistence
		}
	}
	if state.currentExisted && s.validRegularPNG(s.path(rollbackCurrentName), screen) {
		state.baseline = s.path(rollbackCurrentName)
	} else if state.previousExisted && s.validRegularPNG(s.path(rollbackPreviousName), screen) {
		state.baseline = s.path(rollbackPreviousName)
	}
	if err := s.writeTransactionPhase(phasePrepared); err != nil {
		return rollbackState{}, err
	}
	return state, nil
}

func (s *DisplayStore) writeTransactionPhase(phase string) error {
	file, err := os.CreateTemp(s.Dir, "transaction-phase-*.tmp")
	if err != nil {
		return ErrPersistence
	}
	temporary := file.Name()
	abandon := func() error {
		_ = file.Close()
		_ = os.Remove(temporary)
		return ErrPersistence
	}
	if _, err := file.WriteString(phase); err != nil {
		return abandon()
	}
	if err := file.Sync(); err != nil {
		return abandon()
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return ErrPersistence
	}
	if err := s.rename(temporary, s.path(transactionPhaseName)); err != nil {
		_ = os.Remove(temporary)
		return ErrPersistence
	}
	// Persist every phase rename, not just the initial marker. The old complete
	// phase therefore survives until the new complete phase is durable; a crash
	// can never turn "displayed" into an empty or partially written state.
	if err := syncDir(s.Dir); err != nil {
		return ErrPersistence
	}
	return nil
}

func (s *DisplayStore) transactionPhase() string {
	payload, err := os.ReadFile(s.path(transactionPhaseName))
	if err != nil {
		return ""
	}
	return string(payload)
}

func (s *DisplayStore) snapshotCanonical(sourceName, snapshotName string) (bool, error) {
	source := s.path(sourceName)
	info, err := os.Lstat(source)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, ErrPersistence
	}
	if !info.Mode().IsRegular() {
		return false, ErrPersistence
	}
	target := s.path(snapshotName)
	if err := s.link(source, target); err == nil {
		return true, nil
	} else if !linkUnsupported(err) {
		return false, ErrPersistence
	}
	if err := copyDurableFile(source, target); err != nil {
		return false, err
	}
	return true, nil
}

// validRegularPNG re-checks a frame this store wrote earlier.
//
// It deliberately does not decode. Every frame reaches the disk only after
// validateFullScreenPNG has decoded it in full, so what has to be established
// here is narrower: that the bytes are still the bytes we wrote, and that the
// geometry they declare is still the geometry of the panel in front of us. A
// PNG carries a CRC32 over every chunk, which answers the first question at
// memory bandwidth instead of at decode speed, and IHDR answers the second.
//
// The distinction matters because this runs up to five times inside a single
// display transaction — twice to choose a rollback baseline, three more times
// when reconciling an interrupted one — and a full-screen decode is one of the
// most expensive things this program does.
func (s *DisplayStore) validRegularPNG(path string, screen ScreenCapabilities) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	payload, err := os.ReadFile(path)
	return err == nil && verifyStoredFullScreenPNG(payload, screen) == nil
}

func (s *DisplayStore) reconcilePostDisplay(payload []byte, screen ScreenCapabilities, candidate string, rollback rollbackState, show func(context.Context, string) error, cause error) (DisplayResult, error) {
	if rollback.baseline == "" {
		// A first display has no previous screen to restore. Complete the durable
		// commit if possible; reporting success is the only way the status store
		// can truthfully advance to the screen that is already visible.
		if _, err := os.Lstat(candidate); err == nil {
			if err := s.rename(candidate, s.path(currentImageName)); err != nil {
				return DisplayResult{}, errors.Join(ErrPersistence, cause, err)
			}
		}
		if err := s.syncDirectory(); err != nil {
			return DisplayResult{}, errors.Join(ErrPersistence, cause, err)
		}
		if err := s.writeTransactionPhase(phaseDurable); err != nil {
			return DisplayResult{}, errors.Join(ErrPersistence, cause, err)
		}
		s.cleanupActiveTransaction()
		s.cleanupPendingDisplays("")
		digest := sha256.Sum256(payload)
		return DisplayResult{SHA256: hex.EncodeToString(digest[:]), DisplayedAt: time.Now().UTC()}, nil
	}

	_, err := s.preserveDisplayed(candidate)
	if err != nil {
		return DisplayResult{}, errors.Join(ErrPersistence, cause, err)
	}
	if err := s.restoreRollback(rollback); err != nil {
		return DisplayResult{}, errors.Join(ErrPersistence, cause, err)
	}
	recoveryContext, cancel := context.WithTimeout(context.Background(), rollbackDisplayTimeout)
	defer cancel()
	if err := show(recoveryContext, s.path(currentImageName)); err != nil {
		// Canonical A/B have already been restored. The uniquely named pending
		// copy keeps the still-visible candidate safe while a later restart or
		// request retries recovery.
		s.cleanupActiveTransaction()
		return DisplayResult{}, errors.Join(ErrPersistence, cause, err)
	}
	s.cleanupActiveTransaction()
	s.cleanupPendingDisplays("")
	return DisplayResult{}, errors.Join(ErrPersistence, cause)
}

func (s *DisplayStore) preserveDisplayed(candidate string) (string, error) {
	source := candidate
	if info, err := os.Lstat(candidate); err != nil || !info.Mode().IsRegular() {
		source = s.path(currentImageName)
	}
	handle, err := os.CreateTemp(s.Dir, pendingDisplayPrefix+"*.png")
	if err != nil {
		return "", ErrPersistence
	}
	pending := handle.Name()
	if err := handle.Close(); err != nil {
		_ = os.Remove(pending)
		return "", ErrPersistence
	}
	if err := os.Remove(pending); err != nil {
		return "", ErrPersistence
	}
	if err := copyDurableFile(source, pending); err != nil {
		return "", err
	}
	if err := syncDir(s.Dir); err != nil {
		return "", ErrPersistence
	}
	// At most one pending copy is ever meaningful: it is the frame that may
	// still be on the panel, and a newer transaction supersedes any older one
	// by definition. Dropping the rest here — rather than only on the paths
	// that go on to succeed — is what bounds this.
	//
	// The unbounded version was a slow disk leak on a narrow but real path: the
	// panel accepts a frame, a later durable step fails, and then the rollback
	// re-display fails too. That path preserves a copy and returns without
	// reaching any cleanup, so each occurrence left roughly 200KB behind on the
	// small root partition that also holds the token and the activity record.
	// A first-show failure short-circuits well before here and never creates a
	// copy at all, which is why this went unnoticed: the obvious "the display
	// is broken" scenario is not the one that leaks.
	s.cleanupPendingDisplays(pending)
	return pending, nil
}

func (s *DisplayStore) restoreRollback(state rollbackState) error {
	// baseline is always the newest verified screen. It may be previous when a
	// corrupt current file existed, so existence alone must never select the
	// rollback source.
	if err := s.installSnapshot(state.baseline, s.path(currentImageName)); err != nil {
		return err
	}
	if state.previousExisted {
		if err := s.installSnapshot(s.path(rollbackPreviousName), s.path(previousImageName)); err != nil {
			return err
		}
	} else if err := os.Remove(s.path(previousImageName)); err != nil && !os.IsNotExist(err) {
		return ErrPersistence
	}
	if err := s.syncDirectory(); err != nil {
		return ErrPersistence
	}
	return nil
}

func (s *DisplayStore) installSnapshot(source, target string) error {
	temporary, err := os.CreateTemp(s.Dir, "transaction-restore-*.png")
	if err != nil {
		return ErrPersistence
	}
	name := temporary.Name()
	abandon := func() error {
		_ = temporary.Close()
		_ = os.Remove(name)
		return ErrPersistence
	}
	input, err := os.Open(source)
	if err != nil {
		return abandon()
	}
	if _, err := io.Copy(temporary, input); err != nil {
		_ = input.Close()
		return abandon()
	}
	if err := input.Close(); err != nil {
		return abandon()
	}
	if err := temporary.Chmod(0600); err != nil {
		return abandon()
	}
	if err := temporary.Sync(); err != nil {
		return abandon()
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(name)
		return ErrPersistence
	}
	if err := s.rename(name, target); err != nil {
		_ = os.Remove(name)
		return ErrPersistence
	}
	return nil
}

func (s *DisplayStore) cleanupActiveTransaction() {
	for _, name := range []string{candidateImageName, stagingImageName, rollbackCurrentName, rollbackPreviousName, transactionPhaseName} {
		_ = os.Remove(s.path(name))
	}
	entries, err := os.ReadDir(s.Dir)
	if err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "transaction-restore-") || strings.HasPrefix(entry.Name(), "transaction-phase-") {
				_ = os.Remove(s.path(entry.Name()))
			}
		}
	}
	_ = syncDir(s.Dir)
}

func (s *DisplayStore) cleanupPendingDisplays(keep string) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := s.path(entry.Name())
		if strings.HasPrefix(entry.Name(), pendingDisplayPrefix) && path != keep {
			_ = os.Remove(path)
		}
	}
	// As above: removing superseded pending copies needs no durability.
}

func (s *DisplayStore) link(oldPath, newPath string) error {
	if s.hooks.linkFile != nil {
		return s.hooks.linkFile(oldPath, newPath)
	}
	return os.Link(oldPath, newPath)
}

func (s *DisplayStore) rename(oldPath, newPath string) error {
	if s.hooks.renameFile != nil {
		return s.hooks.renameFile(oldPath, newPath)
	}
	return os.Rename(oldPath, newPath)
}

func (s *DisplayStore) syncDirectory() error {
	if s.hooks.syncDirectory != nil {
		return s.hooks.syncDirectory(s.Dir)
	}
	return syncDir(s.Dir)
}

func copyDurableFile(source, target string) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return ErrPersistence
	}
	input, err := os.Open(source)
	if err != nil {
		return ErrPersistence
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrPersistence
	}
	abandon := func() error {
		_ = output.Close()
		_ = os.Remove(target)
		return ErrPersistence
	}
	if _, err := io.Copy(output, input); err != nil {
		return abandon()
	}
	if err := output.Sync(); err != nil {
		return abandon()
	}
	if err := output.Close(); err != nil {
		_ = os.Remove(target)
		return ErrPersistence
	}
	return nil
}

func linkUnsupported(err error) bool {
	return errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EOPNOTSUPP)
}

func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := handle.Sync()
	closeErr := handle.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func runHook(hook func() error) error {
	if hook == nil {
		return nil
	}
	return hook()
}

// validateFullScreenPNG refuses anything that is not a fully decodable
// grayscale image matching the probed geometry. The cheap geometry check runs
// first; the full decode then proves the pixel data itself is intact, which is
// what makes a truncated or corrupted file detectable rather than displayable.
func validateFullScreenPNG(payload []byte, screen ScreenCapabilities) error {
	config, err := png.DecodeConfig(bytes.NewReader(payload))
	if err != nil || config.Width != screen.Width || config.Height != screen.Height {
		return ErrDecodeFailed
	}
	decoded, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		return ErrDecodeFailed
	}
	if _, ok := decoded.(*image.Gray); !ok {
		return ErrDecodeFailed
	}
	return nil
}

// pngChunkOverhead is the per-chunk length, type and CRC framing.
const pngChunkOverhead = 4 + 4 + 4

// verifyStoredFullScreenPNG proves a stored frame is intact and still matches
// the panel, without materialising a single pixel.
//
// It walks the chunk structure, checks the CRC32 the format already carries for
// each chunk, and reads the geometry and colour model out of IHDR. Truncation,
// a flipped bit, a rewritten header and a frame left over from a different
// panel geometry are all caught. What it does not re-establish is that the
// compressed image data decodes to the declared number of scanlines — that was
// established by validateFullScreenPNG before the bytes were ever written, and
// the CRCs prove they have not changed since.
func verifyStoredFullScreenPNG(payload []byte, screen ScreenCapabilities) error {
	if len(payload) < len(pngSignature) || !bytes.Equal(payload[:len(pngSignature)], pngSignature) {
		return ErrDecodeFailed
	}
	offset := len(pngSignature)
	sawHeader, sawData, sawEnd := false, false, false
	for offset < len(payload) {
		if offset+pngChunkOverhead > len(payload) {
			return ErrDecodeFailed
		}
		// The declared length is compared against what is left rather than by
		// adding it to the offset. On the 32-bit ARM target an int is 32 bits,
		// so a chunk claiming close to 2GiB makes `offset + overhead + length`
		// wrap negative, the bounds test pass, and the slice below panic. A
		// corrupt file in the state directory must not be able to take the
		// service down.
		length := int(binary.BigEndian.Uint32(payload[offset : offset+4]))
		if length < 0 || length > len(payload)-offset-pngChunkOverhead {
			return ErrDecodeFailed
		}
		kind := string(payload[offset+4 : offset+8])
		body := payload[offset+8 : offset+8+length]
		recorded := binary.BigEndian.Uint32(payload[offset+8+length : offset+pngChunkOverhead+length])
		// The CRC covers the type and the data, not the length.
		if crc32.ChecksumIEEE(payload[offset+4:offset+8+length]) != recorded {
			return ErrDecodeFailed
		}
		switch kind {
		case "IHDR":
			if sawHeader || length != 13 {
				return ErrDecodeFailed
			}
			sawHeader = true
			width := int64(binary.BigEndian.Uint32(body[0:4]))
			height := int64(binary.BigEndian.Uint32(body[4:8]))
			if width != int64(screen.Width) || height != int64(screen.Height) {
				return ErrDecodeFailed
			}
			// 8-bit grayscale, no interlacing: exactly what this store writes.
			if body[8] != 8 || body[9] != 0 || body[12] != 0 {
				return ErrDecodeFailed
			}
		case "IDAT":
			if !sawHeader {
				return ErrDecodeFailed
			}
			sawData = true
		case "IEND":
			sawEnd = true
		default:
			if !sawHeader {
				return ErrDecodeFailed
			}
		}
		offset += pngChunkOverhead + length
	}
	if !sawHeader || !sawData || !sawEnd || offset != len(payload) {
		return ErrDecodeFailed
	}
	return nil
}

// RecoveredScreen is a validated screen selected by startup recovery.
type RecoveredScreen struct {
	Name    string
	Path    string
	Payload []byte
	SHA256  string
}

// Recover selects the newest screen that still validates. current.png is
// preferred and previous.png is the fallback. When neither validates an error is
// returned instead of a payload: the caller must then leave the panel untouched
// rather than display unvalidated data.
func (s *DisplayStore) Recover(screen ScreenCapabilities) (RecoveredScreen, error) {
	if err := s.reconcileInterruptedTransaction(screen); err != nil {
		return RecoveredScreen{}, err
	}
	for _, name := range []string{currentImageName, previousImageName} {
		path := s.path(name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if err := validateFullScreenPNG(payload, screen); err != nil {
			continue
		}
		digest := sha256.Sum256(payload)
		return RecoveredScreen{
			Name:    name,
			Path:    s.path(name),
			Payload: payload,
			SHA256:  hex.EncodeToString(digest[:]),
		}, nil
	}
	return RecoveredScreen{}, ErrPersistence
}

// reconcileInterruptedTransaction repairs canonical names left by a real
// process crash. A still-named candidate means promotion never completed, so
// both saved generations are restored. Once candidate has been renamed away,
// a valid current is the promoted screen and is kept.
func (s *DisplayStore) reconcileInterruptedTransaction(screen ScreenCapabilities) error {
	candidate := s.path(candidateImageName)
	rollbackCurrent := s.path(rollbackCurrentName)
	rollbackPrevious := s.path(rollbackPreviousName)
	phase := s.transactionPhase()
	candidateValid := s.validRegularPNG(candidate, screen)
	currentSnapshotValid := s.validRegularPNG(rollbackCurrent, screen)
	previousSnapshotValid := s.validRegularPNG(rollbackPrevious, screen)
	currentValid := s.validRegularPNG(s.path(currentImageName), screen)
	previousValid := s.validRegularPNG(s.path(previousImageName), screen)

	// No prepared phase means FBInk was never called: Commit writes and fsyncs
	// this marker before Show. The candidate is never promoted merely because it
	// validates; a uniquely named copy is retained conservatively before active
	// metadata is cleared, so even a damaged marker cannot erase visible bytes.
	if phase == "" {
		if candidateValid {
			if _, err := s.preserveDisplayed(candidate); err != nil {
				return err
			}
		}
		s.cleanupActiveTransaction()
		if currentValid || previousValid {
			return nil
		}
		return ErrPersistence
	}
	if phase == phaseDurable {
		// The transaction completed, so current is the promoted screen. If its
		// bytes did not survive the interruption, previous still holds the screen
		// rotated out of current by this very transaction: the A/B fallback stays
		// available and must be used rather than refusing to restore anything.
		if !currentValid && !previousValid {
			return ErrPersistence
		}
		s.cleanupActiveTransaction()
		return nil
	}
	baseline := ""
	if currentSnapshotValid {
		baseline = rollbackCurrent
	} else if previousSnapshotValid {
		baseline = rollbackPrevious
	}
	if phase != phasePrepared && phase != phaseDisplayed && phase != phaseRotated && phase != phasePromoted {
		if candidateValid || currentValid {
			source := candidate
			if !candidateValid {
				source = s.path(currentImageName)
			}
			if _, err := s.preserveDisplayed(source); err != nil {
				return err
			}
		}
		if baseline != "" {
			previousInfo, previousErr := os.Lstat(rollbackPrevious)
			state := rollbackState{
				currentExisted:  currentSnapshotValid,
				previousExisted: previousErr == nil && previousInfo.Mode().IsRegular(),
				baseline:        baseline,
			}
			if err := s.restoreRollback(state); err != nil {
				return err
			}
		}
		s.cleanupActiveTransaction()
		if baseline != "" {
			return nil
		}
		return ErrPersistence
	}
	if baseline != "" {
		pending, err := s.preserveDisplayed(candidate)
		if err != nil {
			return err
		}
		previousInfo, previousErr := os.Lstat(rollbackPrevious)
		state := rollbackState{
			currentExisted:  currentSnapshotValid,
			previousExisted: previousErr == nil && previousInfo.Mode().IsRegular(),
			baseline:        baseline,
		}
		if err := s.restoreRollback(state); err != nil {
			return err
		}
		s.cleanupActiveTransaction()
		// The panel may still contain the interrupted candidate until the caller
		// re-displays the returned canonical screen. Preserve its unique copy.
		s.cleanupPendingDisplays(pending)
		return nil
	}

	// With no old verified screen, only a durable "displayed" (or later) phase
	// proves that FBInk succeeded. A merely prepared candidate is never promoted.
	if phase == phasePrepared {
		if candidateValid {
			if _, err := s.preserveDisplayed(candidate); err != nil {
				return err
			}
		}
		s.cleanupActiveTransaction()
		return ErrPersistence
	}
	if candidateValid {
		if err := s.rename(candidate, s.path(currentImageName)); err != nil {
			return ErrPersistence
		}
	} else if !currentValid {
		return ErrPersistence
	}
	if err := s.syncDirectory(); err != nil {
		return ErrPersistence
	}
	if err := s.writeTransactionPhase(phaseDurable); err != nil {
		return ErrPersistence
	}
	s.cleanupActiveTransaction()
	return nil
}
