package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/image/font/gofont/goregular"
)

// Every digest in these tests is computed from the bytes under test at run
// time. No checksum is ever hard-coded, so the tests cannot drift into
// asserting a fabricated value.
func fontDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func fontEntryFor(role, file string, payload []byte) FontEntry {
	return FontEntry{
		Role: role,
		// Family and Style together are what identify a face in a licence
		// record; a manifest that names the family but not the style does not
		// say which of a dozen weights was actually redistributed, so the
		// fixture pins both and every test inherits that.
		Family:     "Test Sans",
		Style:      "Regular",
		Version:    "1.0",
		File:       file,
		URL:        "https://fonts.example.org/" + file,
		SHA256:     fontDigest(payload),
		Bytes:      int64(len(payload)),
		License:    "OFL-1.1",
		LicenseURL: "https://example.org/OFL-1.1.txt",
	}
}

func marshalManifest(t *testing.T, entries ...FontEntry) []byte {
	t.Helper()
	raw, err := json.Marshal(FontManifest{SchemaVersion: 1, Fonts: entries})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func countingFetcher(payload []byte, calls *int) FontFetcher {
	return func(context.Context, string) (io.ReadCloser, error) {
		*calls++
		return io.NopCloser(bytes.NewReader(payload)), nil
	}
}

func assertNoTemporaryFonts(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("a half-installed font was left behind: %s", entry.Name())
		}
	}
}

func TestFontManifestRefusesUnsafeEntries(t *testing.T) {
	payload := []byte("a font payload")
	valid := fontEntryFor(fontRoleRegular, "test-regular.ttf", payload)
	if _, err := ParseFontManifest(marshalManifest(t, valid)); err != nil {
		t.Fatalf("a valid manifest was rejected: %v", err)
	}

	mutations := map[string]func(FontEntry) FontEntry{
		"unknown role":    func(e FontEntry) FontEntry { e.Role = "display"; return e },
		"path traversal":  func(e FontEntry) FontEntry { e.File = "../token"; return e },
		"nested path":     func(e FontEntry) FontEntry { e.File = "sub/dir.ttf"; return e },
		"dot file":        func(e FontEntry) FontEntry { e.File = ".hidden.ttf"; return e },
		"short digest":    func(e FontEntry) FontEntry { e.SHA256 = "abcd"; return e },
		"non hex digest":  func(e FontEntry) FontEntry { e.SHA256 = strings.Repeat("z", 64); return e },
		"plaintext url":   func(e FontEntry) FontEntry { e.URL = "http://fonts.example.org/x.ttf"; return e },
		"file url":        func(e FontEntry) FontEntry { e.URL = "file:///etc/passwd"; return e },
		"missing licence": func(e FontEntry) FontEntry { e.License = ""; return e },
		// A blank style is as unusable as a blank family: "Test Sans" alone
		// does not identify the file whose digest is pinned below it, so it is
		// refused at parse time rather than surfacing later as an unattributed
		// face in the third-party record.
		"missing style":    func(e FontEntry) FontEntry { e.Style = ""; return e },
		"missing family":   func(e FontEntry) FontEntry { e.Family = ""; return e },
		"zero size":        func(e FontEntry) FontEntry { e.Bytes = 0; return e },
		"implausible size": func(e FontEntry) FontEntry { e.Bytes = maxFontBytes + 1; return e },
		"no regular":       func(e FontEntry) FontEntry { e.Role = fontRoleBold; return e },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseFontManifest(marshalManifest(t, mutate(valid))); err == nil {
				t.Fatal("an unsafe manifest entry was accepted")
			}
		})
	}

	t.Run("duplicate role", func(t *testing.T) {
		if _, err := ParseFontManifest(marshalManifest(t, valid, valid)); err == nil {
			t.Fatal("a duplicated role was accepted")
		}
	})
	t.Run("unknown field", func(t *testing.T) {
		if _, err := ParseFontManifest([]byte(`{"schema_version":1,"fonts":[],"subset":true}`)); err == nil {
			t.Fatal("an unknown manifest field was accepted")
		}
	})
	t.Run("future schema", func(t *testing.T) {
		if _, err := ParseFontManifest([]byte(`{"schema_version":2,"fonts":[]}`)); err == nil {
			t.Fatal("a future schema version was accepted")
		}
	})
}

func TestEnsureFontsInstallsAtomicallyAndReusesVerifiedFiles(t *testing.T) {
	directory := t.TempDir()
	payload := []byte("the pinned font bytes")
	manifest, err := ParseFontManifest(marshalManifest(t, fontEntryFor(fontRoleRegular, "pinned.ttf", payload)))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := EnsureFonts(context.Background(), manifest, directory, countingFetcher(payload, &calls), nil); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	installed, err := os.ReadFile(filepath.Join(directory, "pinned.ttf"))
	if err != nil || !bytes.Equal(installed, payload) {
		t.Fatalf("the font was not installed: %v", err)
	}
	assertNoTemporaryFonts(t, directory)
	if calls != 1 {
		t.Fatalf("expected exactly one download, got %d", calls)
	}

	// A second run must be free: the digest already matches, so nothing is
	// fetched and nothing is rewritten.
	if err := EnsureFonts(context.Background(), manifest, directory, countingFetcher(payload, &calls), nil); err != nil {
		t.Fatalf("the repeated ensure failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("a verified font was re-downloaded; calls=%d", calls)
	}
}

func TestEnsureFontsFailsClosedWithoutLeavingAHalfInstall(t *testing.T) {
	payload := []byte("the pinned font bytes")
	entry := fontEntryFor(fontRoleRegular, "pinned.ttf", payload)
	manifest, err := ParseFontManifest(marshalManifest(t, entry))
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string][]byte{
		"tampered body":  []byte("the WRONG font bytes!"),
		"truncated body": payload[:len(payload)-1],
		"oversized body": append(append([]byte{}, payload...), 'x'),
	}
	for name, served := range cases {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			calls := 0
			err := EnsureFonts(context.Background(), manifest, directory, countingFetcher(served, &calls), nil)
			if err == nil {
				t.Fatal("a font that does not match the manifest was installed")
			}
			if _, statErr := os.Stat(filepath.Join(directory, "pinned.ttf")); statErr == nil {
				t.Fatal("the target file was published despite the failure")
			}
			assertNoTemporaryFonts(t, directory)
		})
	}

	t.Run("unreachable source", func(t *testing.T) {
		directory := t.TempDir()
		failing := func(context.Context, string) (io.ReadCloser, error) { return nil, errors.New("no network") }
		if err := EnsureFonts(context.Background(), manifest, directory, failing, nil); err == nil {
			t.Fatal("an unreachable source was treated as success")
		}
		assertNoTemporaryFonts(t, directory)
	})
}

func TestFontLibraryFailsClosedOnMissingOrMismatchedFiles(t *testing.T) {
	directory := t.TempDir()
	entry := fontEntryFor(fontRoleRegular, "regular.ttf", goregular.TTF)
	manifest, err := ParseFontManifest(marshalManifest(t, entry))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "regular.ttf")

	if _, err := LoadFontLibrary(manifest, directory); !errors.Is(err, ErrFontMissing) {
		t.Fatalf("a missing font did not fail closed: %v", err)
	}

	if err := os.WriteFile(target, goregular.TTF, 0644); err != nil {
		t.Fatal(err)
	}
	library, err := LoadFontLibrary(manifest, directory)
	if err != nil {
		t.Fatalf("a verified font did not load: %v", err)
	}
	if _, err := library.Face(fontRoleRegular, 18); err != nil {
		t.Fatalf("the regular face was not produced: %v", err)
	}
	// An absent role degrades to regular rather than to an empty face.
	if _, err := library.Face(fontRoleMono, 18); err != nil {
		t.Fatalf("the fallback face was not produced: %v", err)
	}
	library.Close()

	// Same length, different bytes: only the digest can catch this, which is
	// exactly the tampering the loader has to refuse.
	tampered := append([]byte{}, goregular.TTF...)
	tampered[len(tampered)/2] ^= 0xff
	if err := os.WriteFile(target, tampered, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFontLibrary(manifest, directory); !errors.Is(err, ErrFontDigest) {
		t.Fatalf("a tampered font did not fail closed: %v", err)
	}
}

// writeFontFile drops a payload into dir under the entry's file name and hands
// back the path the loader will be pointed at.
func writeFontFile(t *testing.T, dir, name string, payload []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, payload, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The bytes handed back must be the bytes that were hashed, and they must be a
// private copy. If the caller received a window onto the file (or a slice the
// verifier still shares), the digest would only describe what the file happened
// to hold at hashing time.
func TestReadVerifiedFontFileReturnsTheBytesItHashed(t *testing.T) {
	directory := t.TempDir()
	payload := []byte("the pinned font bytes, verbatim")
	entry := fontEntryFor(fontRoleRegular, "regular.ttf", payload)
	path := writeFontFile(t, directory, entry.File, payload)

	verified, err := readVerifiedFontFile(path, entry)
	if err != nil {
		t.Fatalf("a file that matches the manifest was refused: %v", err)
	}
	if !bytes.Equal(verified, payload) {
		t.Fatalf("the verified buffer is not the file content: %q", verified)
	}
	// Hashing the returned slice, rather than trusting the function's own
	// bookkeeping, is what makes this an independent check of the pin.
	if digest := fontDigest(verified); digest != entry.SHA256 {
		t.Fatalf("the returned bytes hash to %s, the manifest pins %s", digest, entry.SHA256)
	}

	// The caller owning the buffer outright: writing through it must not reach
	// the installed file.
	verified[0] ^= 0xff
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, payload) {
		t.Fatal("the returned buffer aliases the installed file; a caller could rewrite a verified font in place")
	}
}

// This is the time-of-check/time-of-use regression. A verify-then-reopen loader
// hashes one read and parses another, so a file swapped between the two is
// vouched for by a digest that never covered it. The construction below is
// deterministic on purpose: no timing window is raced, the swap simply happens
// after the verified read has returned, and the invariant is that the buffer
// already in hand cannot be affected by it.
func TestReadVerifiedFontFileHashesExactlyTheBytesItReturns(t *testing.T) {
	directory := t.TempDir()
	payload := append([]byte{}, goregular.TTF...)
	entry := fontEntryFor(fontRoleRegular, "regular.ttf", payload)
	path := writeFontFile(t, directory, entry.File, payload)

	verified, err := readVerifiedFontFile(path, entry)
	if err != nil {
		t.Fatalf("the pinned font was refused: %v", err)
	}

	// Same length, different content: a size check cannot see this, and a
	// loader that re-opens the path after verifying would parse it happily.
	swapped := append([]byte{}, payload...)
	swapped[len(swapped)/2] ^= 0xff
	if len(swapped) != len(payload) {
		t.Fatal("the swap must not change the file size, otherwise it proves nothing about the digest")
	}
	if err := os.WriteFile(path, swapped, 0644); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(verified, payload) {
		t.Fatal("the verified buffer followed the file on disk; the digest vouched for bytes the caller never received")
	}
	if digest := fontDigest(verified); digest != entry.SHA256 {
		t.Fatalf("the buffer in hand no longer matches the pin it was verified against: %s", digest)
	}

	// A fresh read of the swapped file has to fail, so the swap is refused
	// rather than merely being invisible to the earlier caller.
	if _, err := readVerifiedFontFile(path, entry); !errors.Is(err, ErrFontDigest) {
		t.Fatalf("the swapped file was accepted on a fresh read: %v", err)
	}
	// And the library built on top of it fails closed for the same reason,
	// rather than parsing a face nobody hashed.
	manifest, err := ParseFontManifest(marshalManifest(t, entry))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFontLibrary(manifest, directory); !errors.Is(err, ErrFontDigest) {
		t.Fatalf("the library loaded a font that does not match the manifest: %v", err)
	}

	// The digest that decides belongs to the handle that was opened, not to the
	// entry or the file name: a good read must not license a later bad one.
	if err := os.WriteFile(path, payload, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readVerifiedFontFile(path, entry); err != nil {
		t.Fatalf("the restored file was refused: %v", err)
	}
	decoy := writeFontFile(t, t.TempDir(), entry.File, swapped)
	if _, err := readVerifiedFontFile(decoy, entry); !errors.Is(err, ErrFontDigest) {
		t.Fatalf("a decoy of the same length was accepted after a good read of the real file: %v", err)
	}
}

// Everything that is not exactly the pinned file is refused, and the two
// failure modes stay distinguishable: "not installed" is ErrFontMissing, "there
// but wrong" is ErrFontDigest. An operator triages on that difference.
func TestReadVerifiedFontFileRefusesAnythingButThePinnedFile(t *testing.T) {
	payload := []byte("the pinned font bytes")
	entry := fontEntryFor(fontRoleRegular, "regular.ttf", payload)

	cases := map[string]struct {
		want  error
		build func(t *testing.T, dir string) string
	}{
		"absent file": {
			want:  ErrFontMissing,
			build: func(t *testing.T, dir string) string { return filepath.Join(dir, entry.File) },
		},
		"symlink to a valid font": {
			// The target verifies perfectly; the link is still refused, because
			// following it would let whatever the link points at be swapped for
			// something else without touching the font directory.
			want: ErrFontMissing,
			build: func(t *testing.T, dir string) string {
				target := writeFontFile(t, dir, "real.ttf", payload)
				link := filepath.Join(dir, entry.File)
				if err := os.Symlink(target, link); err != nil {
					t.Skipf("symlinks are unavailable on this filesystem: %v", err)
				}
				return link
			},
		},
		"directory in place of the file": {
			want: ErrFontMissing,
			build: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, entry.File)
				if err := os.Mkdir(path, 0755); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		"truncated file": {
			want: ErrFontDigest,
			build: func(t *testing.T, dir string) string {
				return writeFontFile(t, dir, entry.File, payload[:len(payload)-1])
			},
		},
		"right size, wrong bytes": {
			want: ErrFontDigest,
			build: func(t *testing.T, dir string) string {
				swapped := append([]byte{}, payload...)
				swapped[0] ^= 0xff
				return writeFontFile(t, dir, entry.File, swapped)
			},
		},
		"one byte too long": {
			// Caught by the size check rather than by the trailing-byte read: a
			// file that is already too long when it is fstatted never gets as far
			// as the tail. That branch only fires on a file that grows mid-read,
			// which cannot be staged deterministically here.
			want: ErrFontDigest,
			build: func(t *testing.T, dir string) string {
				return writeFontFile(t, dir, entry.File, append(append([]byte{}, payload...), 'x'))
			},
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			path := testCase.build(t, t.TempDir())
			got, err := readVerifiedFontFile(path, entry)
			if !errors.Is(err, testCase.want) {
				t.Fatalf("expected %v, got %v", testCase.want, err)
			}
			if got != nil {
				t.Fatalf("a refused font still handed back %d bytes", len(got))
			}
		})
	}
}

func TestRunFontsEnsureVerifiesAnInstalledSet(t *testing.T) {
	directory := t.TempDir()
	entry := fontEntryFor(fontRoleRegular, "regular.ttf", goregular.TTF)
	raw := marshalManifest(t, entry)
	if err := os.WriteFile(filepath.Join(directory, fontManifestName), raw, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "regular.ttf"), goregular.TTF, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EINKRELAY_FONT_DIR", directory)

	var stdout, stderr bytes.Buffer
	// Everything already verifies, so this path never reaches the network.
	if code := runFonts(context.Background(), []string{"ensure"}, &stdout, &stderr); code != 0 {
		t.Fatalf("ensure failed: code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "fonts verified") {
		t.Fatalf("unexpected output: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runFonts(context.Background(), []string{"install"}, &stdout, &stderr); code != 2 {
		t.Fatalf("an unknown fonts subcommand returned %d", code)
	}

	if err := os.Remove(filepath.Join(directory, fontManifestName)); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runFonts(context.Background(), []string{"ensure"}, &stdout, &stderr); code != 1 {
		t.Fatalf("a missing manifest returned %d", code)
	}
}

// A Kindle that never joins a network cannot be fixed by retrying, so a failed
// ensure has to say how to preseed the font from a host. Both failure paths
// need it: the install can fail before anything is downloaded, and the load can
// fail on files that are already sitting in the directory.
//
// Everything here is hermetic. The install path is made to fail before any
// fetch is attempted, and the load path is driven by a file that verifies
// against its pin but is not a parseable face, so neither case touches the
// network.
func TestRunFontsEnsurePrintsTheOfflinePreseedGuidance(t *testing.T) {
	// Guard the hint itself first: an implementation that satisfied the stderr
	// assertions with a vacuous string would help nobody.
	for _, fragment := range []string{"offline install", "sha256sum", "EINKRELAY_FONT_DIR", "eink-relay fonts ensure", defaultFontDir} {
		if !strings.Contains(fontOfflinePreseedHint, fragment) {
			t.Fatalf("the preseed hint no longer explains %q: %q", fragment, fontOfflinePreseedHint)
		}
	}

	assertGuidance := func(t *testing.T, stderr string, secrets ...string) {
		t.Helper()
		if !strings.Contains(stderr, fontOfflinePreseedHint) {
			t.Fatalf("the failure did not print the offline preseed guidance: %q", stderr)
		}
		for _, fragment := range []string{"offline install", "sha256sum", "EINKRELAY_FONT_DIR", "eink-relay fonts ensure"} {
			if !strings.Contains(stderr, fragment) {
				t.Fatalf("the guidance is missing %q: %q", fragment, stderr)
			}
		}
		// The hint names the device directory on purpose; it must still not
		// name where this host happens to keep its files.
		for _, secret := range secrets {
			if secret != "" && strings.Contains(stderr, secret) {
				t.Fatalf("the failure output leaked a host path %q: %q", secret, stderr)
			}
		}
	}

	t.Run("installation fails", func(t *testing.T) {
		root := t.TempDir()
		// The font directory cannot be created because its parent is a regular
		// file, so EnsureFonts fails before it would reach for the network.
		blocker := filepath.Join(root, "not-a-directory")
		if err := os.WriteFile(blocker, []byte("occupied"), 0644); err != nil {
			t.Fatal(err)
		}
		manifestPath := filepath.Join(root, fontManifestName)
		raw := marshalManifest(t, fontEntryFor(fontRoleRegular, "regular.ttf", []byte("the pinned font bytes")))
		if err := os.WriteFile(manifestPath, raw, 0644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("EINKRELAY_FONT_DIR", filepath.Join(blocker, "fonts"))
		t.Setenv("EINKRELAY_FONT_MANIFEST", manifestPath)

		var stdout, stderr bytes.Buffer
		if code := runFonts(context.Background(), []string{"ensure"}, &stdout, &stderr); code != 1 {
			t.Fatalf("a failed installation returned %d; stderr=%q", code, stderr.String())
		}
		assertGuidance(t, stderr.String(), root, manifestPath, blocker)
	})

	t.Run("the installed set does not load", func(t *testing.T) {
		directory := t.TempDir()
		// These bytes hash to their pin, so the ensure step is satisfied and no
		// download happens; they are not a font, so the library load fails.
		payload := []byte("this matches its digest but is not a font")
		entry := fontEntryFor(fontRoleRegular, "regular.ttf", payload)
		if err := os.WriteFile(filepath.Join(directory, fontManifestName), marshalManifest(t, entry), 0644); err != nil {
			t.Fatal(err)
		}
		writeFontFile(t, directory, entry.File, payload)
		t.Setenv("EINKRELAY_FONT_DIR", directory)

		var stdout, stderr bytes.Buffer
		if code := runFonts(context.Background(), []string{"ensure"}, &stdout, &stderr); code != 1 {
			t.Fatalf("an unloadable font set returned %d; stderr=%q", code, stderr.String())
		}
		assertGuidance(t, stderr.String(), directory)
	})
}

// TestFaceCacheIsBounded pins the memory behaviour of the optional font_size
// parameter. Without a bound, every value a caller uses pins a body face, a
// monospace face and six heading faces forever; walking the whole 12..72 range
// took the service from 22.8MB to 56.6MB of resident memory on the device and
// none of it came back.
func TestFaceCacheIsBounded(t *testing.T) {
	library := installedCJKLibrary(t)
	for size := 1; size <= 4*maxCachedFaces; size++ {
		if _, err := library.Face(fontRoleRegular, float64(size)); err != nil {
			t.Fatalf("face at size %d: %v", size, err)
		}
		if len(library.faces) > maxCachedFaces {
			t.Fatalf("cache grew to %d faces, above the bound of %d", len(library.faces), maxCachedFaces)
		}
		if len(library.faces) != len(library.order) {
			t.Fatalf("cache and recency list disagree: %d vs %d", len(library.faces), len(library.order))
		}
	}
}

// TestFaceCacheKeepsASingleDocumentsWorkingSet is the other half of the bound:
// it has to be generous enough that nothing is evicted while one page is being
// laid out and drawn, or the renderer would rebuild faces it is still using.
func TestFaceCacheKeepsASingleDocumentsWorkingSet(t *testing.T) {
	library := installedCJKLibrary(t)
	style := ScaledMarkdownStyle(maxFontSize)
	wanted := []struct {
		role string
		size float64
	}{
		{fontRoleRegular, style.BaseSize},
		{fontRoleBold, style.BaseSize},
		{fontRoleItalic, style.BaseSize},
		{fontRoleMono, style.BaseSize * 0.9},
	}
	for _, size := range style.HeadingSizes {
		wanted = append(wanted, struct {
			role string
			size float64
		}{fontRoleBold, size})
	}
	if len(wanted) > maxCachedFaces {
		t.Fatalf("one document needs %d faces, above the bound of %d", len(wanted), maxCachedFaces)
	}
	for _, want := range wanted {
		if _, err := library.Face(want.role, want.size); err != nil {
			t.Fatal(err)
		}
	}
	// Every face the document asked for is still resident.
	for _, want := range wanted {
		resolved := want.role
		if library.fonts[resolved] == nil {
			resolved = fontRoleRegular
		}
		if _, cached := library.faces[faceKey{role: resolved, size: want.size}]; !cached {
			t.Fatalf("face %s/%v was evicted while the document still needed it", want.role, want.size)
		}
	}
}
