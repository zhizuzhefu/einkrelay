package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSysfsScreenProbeReadsTheVisibleMode(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "modes")
	// Exactly what a PW4 writes: the mode in force first, then the
	// alternatives the panel could be switched to.
	if err := os.WriteFile(path, []byte("U:1072x1448p-0\nU:1072x1448p-0\nU:1448x1072p-0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	capabilities, err := (&SysfsScreenProbe{Path: path}).Probe(context.Background())
	if err != nil || capabilities.Width != 1072 || capabilities.Height != 1448 {
		t.Fatalf("unexpected probe result: %+v %v", capabilities, err)
	}
}

// TestSysfsScreenProbeRejectsTheVirtualGeometry is the regression that matters
// most in this file. The probe used to read `virtual_size`, which on a PW4
// reports 1088x6144: the stride-padded width plus enough rows for several
// stacked buffers. Every frame was then laid out on a canvas 4.2x taller than
// the panel, so an image centred by fit=contain landed entirely below the
// visible rows and a successful request displayed nothing but white. The
// content of that node must never again be accepted as a panel geometry.
func TestSysfsScreenProbeRejectsTheVirtualGeometry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modes")
	if err := os.WriteFile(path, []byte("1088,6144\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if capabilities, err := (&SysfsScreenProbe{Path: path}).Probe(context.Background()); err == nil {
		t.Fatalf("the virtual_size format was accepted as a mode: %+v", capabilities)
	}
}

func TestSysfsScreenProbeFailsClosedOnUnusableGeometry(t *testing.T) {
	for _, raw := range []string{
		"",
		"\n\n",
		"U:",
		"U:1072",
		"U:1072x",
		"U:x1448",
		"U:0x1448p-0",
		"U:1072x0p-0",
		"U:1072xabcp-0",
		"U:99999x1448p-0",
		"U:1072x99999p-0",
		"U:-1x-1p-0",
		"1088,6144",
	} {
		if capabilities, err := parseVideoModes(raw); err == nil {
			t.Fatalf("accepted invalid mode list %q as %+v", raw, capabilities)
		}
	}
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err := (&SysfsScreenProbe{Path: missing}).Probe(context.Background()); err == nil {
		t.Fatal("missing framebuffer node was accepted")
	}
}

// TestSysfsScreenProbeAcceptsAModeLineWithoutADecoration keeps the parser
// tolerant of the shapes fbdev is allowed to write: the `<name>:` prefix and
// the `p-<refresh>` suffix are both optional in the wild.
func TestSysfsScreenProbeAcceptsAModeLineWithoutADecoration(t *testing.T) {
	for _, raw := range []string{"U:1072x1448p-0", "1072x1448", "U:1072x1448", "1072x1448p-0", "  U:1072x1448p-0  "} {
		capabilities, err := parseVideoModes(raw)
		if err != nil || capabilities.Width != 1072 || capabilities.Height != 1448 {
			t.Fatalf("mode %q parsed as %+v (%v)", raw, capabilities, err)
		}
	}
}
