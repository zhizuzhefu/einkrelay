package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

// The font roles the renderer knows about. "regular" is the only mandatory
// one; the others fall back to it so a manifest can ship fewer files without
// silently changing what gets drawn.
const (
	fontRoleRegular = "regular"
	fontRoleBold    = "bold"
	fontRoleItalic  = "italic"
	fontRoleMono    = "mono"
)

const (
	defaultFontDir   = "/mnt/us/einkrelay/fonts"
	fontManifestName = "manifest.json"
	// fontManifestSchema is bumped whenever the on-disk shape changes, so an
	// old manifest fails closed instead of being half-understood.
	fontManifestSchema = 1
	// maxFontBytes bounds a single font file. A CJK face is a few tens of
	// megabytes; anything larger is treated as a mistake rather than streamed
	// onto a device with ~490MB of RAM.
	maxFontBytes int64 = 48 * 1024 * 1024
)

// fontOfflinePreseedHint is printed whenever the fonts cannot be installed or
// loaded on the device. A Kindle that never joins a network can only be fixed
// from a host, and an operator staring at a failed ensure has no reason to go
// looking in the docs for that. It names no host path on purpose: the manifest
// already carries the URL and digest, and the device side is the only part the
// tool can speak to authoritatively.
const fontOfflinePreseedHint = "offline install: on a networked host fetch each `url` from the manifest, check it with `sha256sum` against the manifest `sha256`, copy the file into the font directory (default /mnt/us/einkrelay/fonts, override with EINKRELAY_FONT_DIR), then re-run `eink-relay fonts ensure` to verify it in place"

var (
	// ErrFontManifest reports a manifest that is missing, malformed, or
	// describes something the loader refuses to trust.
	ErrFontManifest = errors.New("font manifest is unusable")
	// ErrFontMissing reports that a pinned font file is absent.
	ErrFontMissing = errors.New("font file is missing")
	// ErrFontDigest reports that a font file is present but does not match the
	// pinned SHA-256. It is deliberately distinct from ErrFontMissing so an
	// operator can tell "not installed yet" from "tampered with or truncated".
	ErrFontDigest = errors.New("font file digest does not match the manifest")
	// ErrFontInstall reports a failed installation. Nothing is ever left
	// half-installed when this is returned.
	ErrFontInstall = errors.New("font installation failed")
)

// FontEntry pins exactly one font file: where it comes from, what it must
// hash to, and under which licence it is redistributed.
type FontEntry struct {
	Role       string `json:"role"`
	Family     string `json:"family"`
	Style      string `json:"style"`
	Version    string `json:"version"`
	File       string `json:"file"`
	URL        string `json:"url"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
	License    string `json:"license"`
	LicenseURL string `json:"license_url"`
}

// FontManifest is the committed record of the bundled fonts. The font binaries
// themselves are never committed; this file plus the ensure subcommand is what
// reproduces them.
type FontManifest struct {
	SchemaVersion int         `json:"schema_version"`
	Fonts         []FontEntry `json:"fonts"`
}

func validFontRole(role string) bool {
	switch role {
	case fontRoleRegular, fontRoleBold, fontRoleItalic, fontRoleMono:
		return true
	}
	return false
}

// ParseFontManifest validates every field before any of them is used to open a
// file or issue a request. Unknown fields are rejected so a manifest written
// for a newer schema cannot be silently misread as a permissive one.
func ParseFontManifest(raw []byte) (FontManifest, error) {
	var manifest FontManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return FontManifest{}, ErrFontManifest
	}
	if decoder.More() {
		return FontManifest{}, ErrFontManifest
	}
	if manifest.SchemaVersion != fontManifestSchema || len(manifest.Fonts) == 0 {
		return FontManifest{}, ErrFontManifest
	}
	seen := make(map[string]bool, len(manifest.Fonts))
	for _, entry := range manifest.Fonts {
		if !validFontRole(entry.Role) || seen[entry.Role] {
			return FontManifest{}, ErrFontManifest
		}
		seen[entry.Role] = true
		if entry.Family == "" || entry.Style == "" || entry.Version == "" || entry.License == "" {
			return FontManifest{}, ErrFontManifest
		}
		// The file name is joined onto the font directory, so it has to be a
		// plain base name: no separators, no parent traversal, no dot files.
		if entry.File == "" || entry.File != filepath.Base(entry.File) || strings.HasPrefix(entry.File, ".") || strings.ContainsAny(entry.File, `/\`) {
			return FontManifest{}, ErrFontManifest
		}
		digest, err := hex.DecodeString(entry.SHA256)
		if err != nil || len(digest) != sha256.Size {
			return FontManifest{}, ErrFontManifest
		}
		parsed, err := url.Parse(entry.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return FontManifest{}, ErrFontManifest
		}
		if entry.Bytes < 1 || entry.Bytes > maxFontBytes {
			return FontManifest{}, ErrFontManifest
		}
	}
	if !seen[fontRoleRegular] {
		return FontManifest{}, ErrFontManifest
	}
	return manifest, nil
}

func LoadFontManifest(path string) (FontManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return FontManifest{}, ErrFontManifest
	}
	return ParseFontManifest(raw)
}

// FontConfig locates the installed fonts. The directory lives on /mnt/us with
// the rest of the large runtime assets, never on the root partition.
type FontConfig struct {
	Dir          string
	ManifestPath string
}

func DefaultFontConfig() FontConfig {
	return FontConfig{Dir: defaultFontDir, ManifestPath: filepath.Join(defaultFontDir, fontManifestName)}
}

func LoadFontConfigFromEnv(getenv func(string) string) (FontConfig, error) {
	cfg := DefaultFontConfig()
	if value := getenv("EINKRELAY_FONT_DIR"); value != "" {
		cfg.Dir = value
		cfg.ManifestPath = filepath.Join(value, fontManifestName)
	}
	if value := getenv("EINKRELAY_FONT_MANIFEST"); value != "" {
		cfg.ManifestPath = value
	}
	if cfg.Dir == "" || cfg.ManifestPath == "" || !filepath.IsAbs(cfg.Dir) || !filepath.IsAbs(cfg.ManifestPath) {
		return FontConfig{}, errors.New("invalid configuration")
	}
	return cfg, nil
}

// verifyFontFile is the single gate every read of an installed font passes
// through. The size check is a cheap pre-filter; the digest is what actually
// decides, and a mismatch is never repaired in place.
func verifyFontFile(path string, entry FontEntry) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ErrFontMissing
	}
	if info.Size() != entry.Bytes {
		return ErrFontDigest
	}
	file, err := os.Open(path)
	if err != nil {
		return ErrFontMissing
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return ErrFontDigest
	}
	if !strings.EqualFold(hex.EncodeToString(hasher.Sum(nil)), entry.SHA256) {
		return ErrFontDigest
	}
	return nil
}

// readVerifiedFontFile returns the bytes it actually hashed. Verifying a file
// and then re-opening it by path leaves a window where the two reads can see
// different content, so the digest would vouch for a byte stream nobody parses;
// here the buffer that is checked is the buffer that is handed back. The size
// comes from the open handle rather than the path for the same reason.
//
// Unlike verifyFontFile this holds the whole face in memory, which is the price
// of the guarantee; the manifest bound on Bytes is what keeps that affordable.
func readVerifiedFontFile(path string, entry FontEntry) ([]byte, error) {
	// ParseFontManifest already bounds Bytes, but this function allocates on the
	// strength of that number, so it does not take a caller's word for it: a
	// hand-built entry must not be able to reserve an arbitrary buffer on a
	// device with ~490MB of RAM.
	if entry.Bytes < 1 || entry.Bytes > maxFontBytes {
		return nil, ErrFontDigest
	}
	// The Lstat is what refuses a symlink: an Open would follow it and the
	// handle would look like a perfectly ordinary regular file.
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrFontMissing
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrFontMissing
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, ErrFontMissing
	}
	if !stat.Mode().IsRegular() {
		return nil, ErrFontMissing
	}
	if stat.Size() != entry.Bytes {
		return nil, ErrFontDigest
	}
	payload := make([]byte, entry.Bytes)
	if _, err := io.ReadFull(file, payload); err != nil {
		return nil, ErrFontDigest
	}
	// A file that still has bytes after the pinned length grew between the
	// fstat and the read, so the digest below would only cover the prefix.
	var tail [1]byte
	if n, _ := file.Read(tail[:]); n != 0 {
		return nil, ErrFontDigest
	}
	sum := sha256.Sum256(payload)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), entry.SHA256) {
		return nil, ErrFontDigest
	}
	return payload, nil
}

// FontFetcher opens the pinned download. It is a parameter so the install path
// can be exercised without a network.
type FontFetcher func(ctx context.Context, source string) (io.ReadCloser, error)

// httpsFontFetcher refuses to leave HTTPS, including across redirects. The
// digest is still the authority; this only avoids handing the request to a
// plaintext hop in the first place.
func httpsFontFetcher(ctx context.Context, source string) (io.ReadCloser, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, ErrFontInstall
	}
	client := &http.Client{
		Timeout: 10 * time.Minute,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= 5 || next.URL.Scheme != "https" {
				return ErrFontInstall
			}
			return nil
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, ErrFontInstall
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, ErrFontInstall
	}
	return response.Body, nil
}

// EnsureFonts brings the font directory up to the manifest. A file that already
// matches its digest is reused untouched, so a repeated run is free and never
// re-downloads. Anything else is installed atomically or not at all.
func EnsureFonts(ctx context.Context, manifest FontManifest, dir string, fetch FontFetcher, log func(string)) error {
	if fetch == nil {
		return ErrFontInstall
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return ErrFontInstall
	}
	note := func(message string) {
		if log != nil {
			log(message)
		}
	}
	for _, entry := range manifest.Fonts {
		target := filepath.Join(dir, entry.File)
		if verifyFontFile(target, entry) == nil {
			note(entry.File + ": already installed and verified")
			continue
		}
		if err := installFont(ctx, entry, dir, target, fetch); err != nil {
			return err
		}
		note(entry.File + ": downloaded and verified")
	}
	return nil
}

// installFont downloads into a temporary file in the destination directory,
// hashes the bytes as they stream past, and only then renames. The rename is
// the single atomic step: an interrupted install leaves the previous file (or
// no file) rather than a truncated face that would render as tofu.
func installFont(ctx context.Context, entry FontEntry, dir, target string, fetch FontFetcher) error {
	body, err := fetch(ctx, entry.URL)
	if err != nil {
		return ErrFontInstall
	}
	defer body.Close()
	file, err := os.CreateTemp(dir, "font-*.tmp")
	if err != nil {
		return ErrFontInstall
	}
	name := file.Name()
	abandon := func(cause error) error {
		_ = file.Close()
		_ = os.Remove(name)
		return cause
	}
	hasher := sha256.New()
	// Reading one byte past the declared size is what turns "the server sent
	// more than the manifest promised" into a refusal instead of an unbounded
	// write onto the device.
	written, err := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(body, entry.Bytes+1))
	if err != nil || written != entry.Bytes {
		return abandon(ErrFontInstall)
	}
	if !strings.EqualFold(hex.EncodeToString(hasher.Sum(nil)), entry.SHA256) {
		return abandon(ErrFontDigest)
	}
	if err := file.Chmod(0644); err != nil {
		return abandon(ErrFontInstall)
	}
	if err := file.Sync(); err != nil {
		return abandon(ErrFontInstall)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return ErrFontInstall
	}
	if err := os.Rename(name, target); err != nil {
		_ = os.Remove(name)
		return ErrFontInstall
	}
	// A rename is directory metadata; without the directory fsync the entry can
	// disappear when a Kindle loses power, which is its normal shutdown.
	if err := syncDir(dir); err != nil {
		return ErrFontInstall
	}
	return nil
}

type faceKey struct {
	role string
	size float64
}

// maxCachedFaces bounds the face cache.
//
// A face is cheap to ask for and expensive to keep: each one carries its own
// rasterisation buffers, and the cache is keyed by size as well as role. The
// optional font_size parameter spans 12..72, and every value of it produces a
// fresh body size, a fresh monospace size and six fresh heading sizes — so a
// caller sizing text responsively walks the whole space and, without a bound,
// pins every face it ever touches for the life of the process.
//
// What the bound is worth was measured rather than assumed, and the honest
// answer is smaller than it first looked. Sweeping all 61 sizes took resident
// memory from 22.8MB to 56.6MB unbounded, and to about 51-54MB with the bound
// in place. The difference is real but modest, because most of that growth is
// the Go heap holding the peak working set of the render path — a full-panel
// canvas plus encoder buffers — rather than the cache: repeating the sweep does
// not grow it further, and hammering a single size adds nothing at all. The
// bound is worth having because a cache keyed on a request parameter with no
// ceiling is a structural defect whatever today's numbers say, not because it
// reclaims tens of megabytes, which it does not.
//
// The bound is set well above what a single document needs. One page uses at
// most the four roles at body size, six heading sizes and monospace — about a
// dozen faces — so nothing is ever evicted mid-render, and the cache still does
// its job of not reparsing a face per paragraph.
const maxCachedFaces = 32

// FontLibrary owns the parsed faces. Faces are produced per size on demand and
// cached, because a heading and a paragraph share one parsed font but need
// different scales.
//
// A FontLibrary is not safe for concurrent use; the display transaction lock
// already serialises rendering.
type FontLibrary struct {
	fonts map[string]*opentype.Font
	faces map[faceKey]font.Face
	// order is the least-recently-used sequence of cached keys, oldest first.
	// A slice rather than a list: the bound is small enough that the linear
	// scan is cheaper than the allocation a linked list would cost.
	order []faceKey
	DPI   float64
}

// NewFontLibrary builds a library straight from font bytes. Production uses
// LoadFontLibrary, which verifies digests first; this entry point exists so
// layout can be tested with a font that is already in the build.
func NewFontLibrary(sources map[string][]byte) (*FontLibrary, error) {
	library := &FontLibrary{fonts: map[string]*opentype.Font{}, faces: map[faceKey]font.Face{}}
	for role, payload := range sources {
		if !validFontRole(role) {
			return nil, ErrFontManifest
		}
		parsed, err := opentype.Parse(payload)
		if err != nil {
			return nil, ErrFontManifest
		}
		library.fonts[role] = parsed
	}
	if library.fonts[fontRoleRegular] == nil {
		return nil, ErrFontMissing
	}
	return library, nil
}

// LoadFontLibrary verifies every pinned file before parsing it. A missing or
// mismatched font fails the whole load: the renderer must never fall back to a
// face that cannot draw the requested script.
func LoadFontLibrary(manifest FontManifest, dir string) (*FontLibrary, error) {
	sources := make(map[string][]byte, len(manifest.Fonts))
	for _, entry := range manifest.Fonts {
		payload, err := readVerifiedFontFile(filepath.Join(dir, entry.File), entry)
		if err != nil {
			return nil, err
		}
		sources[entry.Role] = payload
	}
	return NewFontLibrary(sources)
}

func (l *FontLibrary) dpi() float64 {
	if l.DPI <= 0 {
		return 72
	}
	return l.DPI
}

// Face resolves a role and size. An absent role falls back to regular, which is
// a visual compromise; an absent regular is a hard failure.
func (l *FontLibrary) Face(role string, size float64) (font.Face, error) {
	if l == nil || size <= 0 {
		return nil, ErrFontMissing
	}
	resolved := role
	parsed, ok := l.fonts[resolved]
	if !ok {
		resolved = fontRoleRegular
		parsed, ok = l.fonts[resolved]
	}
	if !ok {
		return nil, ErrFontMissing
	}
	key := faceKey{role: resolved, size: size}
	if face, cached := l.faces[key]; cached {
		l.touch(key)
		return face, nil
	}
	face, err := opentype.NewFace(parsed, &opentype.FaceOptions{Size: size, DPI: l.dpi(), Hinting: font.HintingFull})
	if err != nil {
		return nil, ErrFontManifest
	}
	l.evictOldest()
	l.faces[key] = face
	l.order = append(l.order, key)
	return face, nil
}

// touch moves a key to the most-recently-used end.
func (l *FontLibrary) touch(key faceKey) {
	for index, existing := range l.order {
		if existing == key {
			l.order = append(l.order[:index], l.order[index+1:]...)
			break
		}
	}
	l.order = append(l.order, key)
}

// evictOldest makes room for one more face. The evicted face is closed rather
// than dropped, because what makes it expensive is the buffers it holds, not
// the map entry.
func (l *FontLibrary) evictOldest() {
	for len(l.order) >= maxCachedFaces {
		oldest := l.order[0]
		l.order = l.order[1:]
		if face, ok := l.faces[oldest]; ok {
			_ = face.Close()
			delete(l.faces, oldest)
		}
	}
}

func (l *FontLibrary) Close() error {
	if l == nil {
		return nil
	}
	for key, face := range l.faces {
		_ = face.Close()
		delete(l.faces, key)
	}
	l.order = nil
	return nil
}

// runFonts implements the `fonts ensure` subcommand. It reports failure without
// echoing URLs, paths or digests beyond what the manifest already publishes.
func runFonts(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "ensure" {
		fmt.Fprintln(stderr, "usage: eink-relay fonts ensure")
		return 2
	}
	cfg, err := LoadFontConfigFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(stderr, "configuration is invalid")
		return 2
	}
	manifest, err := LoadFontManifest(cfg.ManifestPath)
	if err != nil {
		fmt.Fprintln(stderr, "the font manifest is unusable")
		return 1
	}
	log := func(message string) { fmt.Fprintln(stdout, message) }
	if err := EnsureFonts(ctx, manifest, cfg.Dir, httpsFontFetcher, log); err != nil {
		fmt.Fprintln(stderr, "installing the fonts failed; nothing was left half-installed")
		fmt.Fprintln(stderr, fontOfflinePreseedHint)
		return 1
	}
	if _, err := LoadFontLibrary(manifest, cfg.Dir); err != nil {
		fmt.Fprintln(stderr, "the installed fonts did not load")
		fmt.Fprintln(stderr, fontOfflinePreseedHint)
		return 1
	}
	fmt.Fprintln(stdout, "fonts verified")
	return 0
}
