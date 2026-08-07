package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The installer is part of the trusted device boundary.  Keep the two
// fail-closed invariants visible in the Go test suite even though the scripts
// themselves intentionally remain BusyBox ash.
func TestInstallScriptsVerifyFontsAndUseFixedUninstallReceipt(t *testing.T) {
	root := filepath.Join("..", "..")
	install, err := os.ReadFile(filepath.Join(root, "scripts", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(install), "fonts ensure") {
		t.Fatal("install.sh must verify pinned fonts before reporting success")
	}
	uninstall, err := os.ReadFile(filepath.Join(root, "scripts", "uninstall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"$install_dir/eink-relay",
		"$install_dir/bin/fbink",
		"$install_dir/fonts/manifest.json",
		"$install_dir/fonts/NotoSansCJKsc-Regular.otf",
		"$install_dir/start.sh",
		"$install_dir/stop.sh",
		"$kual_dir/menu.json",
		"$kual_dir/bin/start.sh",
		"$kual_dir/bin/stop.sh",
		"$state_dir/token",
		"$receipt",
	} {
		if !strings.Contains(string(uninstall), path) {
			t.Fatalf("uninstall.sh must restrict receipts to %q", path)
		}
	}
	for _, name := range []string{"stop.sh", "uninstall.sh"} {
		script, err := os.ReadFile(filepath.Join(root, "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(script)
		if !strings.Contains(text, `id -u`) {
			t.Fatalf("%s must require root", name)
		}
		if strings.Contains(text, "EINKRELAY_INSTALL_") || strings.Contains(text, "EINKRELAY_STATE_DIR") {
			t.Fatalf("%s must keep its uninstall/recovery roots fixed", name)
		}
	}
}

// The installer writes the only token the service will ever accept, so the
// bytes it produces have to be exactly the bytes LoadToken accepts.  A trailing
// newline is invisible in a shell transcript but fails the 0x21-0x7e check, so a
// freshly installed device would exit 1 on every start and then trip the
// Guardian failsafe.  Pin both halves of that contract: the write itself, and
// the loader's verdict on each form.
func TestInstalledTokenBytesLoadOnAFreshDevice(t *testing.T) {
	install, err := os.ReadFile(filepath.Join("..", "..", "scripts", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(install)
	if strings.Contains(text, `printf '%s\n' "$token"`) {
		t.Fatal("install.sh must not terminate the token file with a newline")
	}
	if !strings.Contains(text, `printf '%s' "$token" >"$STATE_DIR/token"`) {
		t.Fatal("install.sh must write the token bytes verbatim")
	}

	// `od -An -N32 -tx1 | tr -d ' \n'` yields exactly 64 lowercase hex digits.
	token := strings.Repeat("0123456789abcdef", 4)
	if len(token) != 64 {
		t.Fatalf("fixture is not the installed token width: %d", len(token))
	}

	installed := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(installed, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadToken(installed)
	if err != nil {
		t.Fatalf("the installed token must load: %v", err)
	}
	if string(loaded) != token {
		t.Fatalf("loader altered the installed token")
	}

	// Guard against the fix being reverted somewhere else: the newline form has
	// to stay rejected, otherwise this test would pass for the wrong reason.
	terminated := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(terminated, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToken(terminated); err == nil {
		t.Fatal("a newline-terminated token must still be refused")
	}
}

// TestKualLauncherEntryIsInstalledAndRemovable covers the gap that made the
// product hard to actually start: the launcher entry lived in the repository
// but nothing installed it, so the only ways in were an SSH session or a manual
// copy. It is installed only when KUAL is already present — EInkRelay does not
// depend on it — and whatever the installer writes must be removable by the
// receipt-driven uninstall.
func TestKualLauncherEntryIsInstalledAndRemovable(t *testing.T) {
	root := filepath.Join("..", "..")
	install, err := os.ReadFile(filepath.Join(root, "scripts", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(install)
	// Never create the extensions root: on a device without KUAL that would
	// leave a directory nothing owns.
	if !strings.Contains(text, `[ -d "$KUAL_EXT_ROOT" ]`) {
		t.Fatal("install.sh must only add the launcher entry when KUAL is already present")
	}
	if strings.Contains(text, `mkdir -p "$KUAL_EXT_ROOT"`) {
		t.Fatal("install.sh must never create the KUAL extensions root itself")
	}
	for _, artifact := range []string{"menu.json", "bin/start.sh", "bin/stop.sh"} {
		if !strings.Contains(text, "$KUAL_EXT_DIR/"+artifact) {
			t.Fatalf("install.sh does not install %q", artifact)
		}
	}

	uninstall, err := os.ReadFile(filepath.Join(root, "scripts", "uninstall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// All three or none: a receipt naming one or two of them was not written by
	// this installer and must be refused outright.
	if !strings.Contains(string(uninstall), `[ "$kual_count" -eq 0 ] || [ "$kual_count" -eq 3 ]`) {
		t.Fatal("uninstall.sh must treat the launcher trio as all-or-nothing")
	}

	// The launcher must run from the installed directory, not from the unpacked
	// release: the package is a staging area an operator may delete.
	launcher, err := os.ReadFile(filepath.Join(root, "extensions", "einkrelay", "bin", "start.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(launcher), "einkrelay-pkg") {
		t.Fatal("the launcher must not depend on the unpacked package directory")
	}
	if !strings.Contains(string(launcher), "APP_DIR=/mnt/us/einkrelay") || !strings.Contains(string(launcher), `"$APP_DIR/start.sh"`) {
		t.Fatal("the launcher must invoke start.sh from the installed directory")
	}

	// pgrep -f matches the shell running the script itself, whose arguments
	// contain the pattern. Resolving the pid from /proc is what keeps a stop
	// from signalling its own session.
	stop, err := os.ReadFile(filepath.Join(root, "extensions", "einkrelay", "bin", "stop.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// Comments are stripped first: the script explains this trap by name, and
	// the explanation is worth keeping.
	var executable []string
	for _, line := range strings.Split(string(stop), "\n") {
		if trimmed := strings.TrimSpace(line); !strings.HasPrefix(trimmed, "#") {
			executable = append(executable, line)
		}
	}
	if strings.Contains(strings.Join(executable, "\n"), "pgrep") {
		t.Fatal("the launcher stop entry must not use pgrep -f: it matches its own command line")
	}
	if !strings.Contains(string(stop), "/proc/") || !strings.Contains(string(stop), "pidof eink-relay") {
		t.Fatal("the launcher stop entry must resolve the guardian pid from /proc")
	}
}

// TestKualStartEntryReentersExclusiveModeAfterAnExit covers the other half of
// "I cannot find a way back in". After a REST or corner-tap exit the Guardian
// is still running and supervising, with the activity record inactive and the
// native interface back on the panel. A launcher that treats "a process
// exists" as "already started" silently does nothing there, which leaves the
// menu entry looking broken. Re-entering means restarting the Guardian,
// because the entry decision is made once at startup from the activity record.
func TestKualStartEntryReentersExclusiveModeAfterAnExit(t *testing.T) {
	launcher, err := os.ReadFile(filepath.Join("..", "..", "extensions", "einkrelay", "bin", "start.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(launcher)
	// The no-op path must be conditioned on the activity record, not merely on
	// a running process.
	if !strings.Contains(text, `"active":[[:space:]]*true`) {
		t.Fatal("the launcher must only treat an already-exclusive device as already started")
	}
	// Anything else has to be restarted, which means terminating the guardian
	// first rather than starting a second one alongside it.
	if !strings.Contains(text, "kill -TERM") {
		t.Fatal("the launcher must stop a non-exclusive guardian before starting a new one")
	}
	if strings.Count(text, "pidof eink-relay") < 2 {
		t.Fatal("the launcher must re-check that the old guardian actually exited")
	}
}

// TestKualStartEntryToleratesTheLaunchWindow covers a race the readiness check
// walked straight into: start.sh runs `eink-relay resume` and only then
// `exec eink-relay guard`, so for a moment after launch no process named
// eink-relay exists. Treating that window as "it exited" made the menu entry
// report failure for a launch that was in fact succeeding — observed on the
// device, where the guardian came up and entered exclusive mode while the
// entry returned 1.
func TestKualStartEntryToleratesTheLaunchWindow(t *testing.T) {
	launcher, err := os.ReadFile(filepath.Join("..", "..", "extensions", "einkrelay", "bin", "start.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(launcher)
	if !strings.Contains(text, "seen=1") {
		t.Fatal("the readiness loop must remember having seen the process before calling its absence a failure")
	}
	if !strings.Contains(text, `elif [ "$seen" -eq 1 ]`) {
		t.Fatal("the readiness loop must only fail on a disappearing process once one has been seen")
	}
}

// TestReleaseBuildStampsTheVersion covers an operational blind spot: without a
// stamp every release reports the compile-time default, `dev`, so /v1/status
// cannot tell a three-week-old install from the one that was just pushed.
func TestReleaseBuildStampsTheVersion(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "release-build.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	if !strings.Contains(text, "-X main.version=") {
		t.Fatal("the release build must stamp main.version")
	}
	if !strings.Contains(text, "--dirty") {
		t.Fatal("a release built from uncommitted work must say so in its version")
	}
	// The stamp reaches the linker as a shell expansion, so it has to be
	// rejected rather than embedded when it is not a plain identifier.
	if !strings.Contains(text, "the derived version stamp is not a plain identifier") {
		t.Fatal("the release build must validate the derived version stamp")
	}
	// The default the stamp replaces has to still exist for non-release builds.
	main, err := os.ReadFile(filepath.Join("cmd", "eink-relay", "main.go"))
	if err != nil {
		main, err = os.ReadFile("main.go")
		if err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(string(main), `var version = "dev"`) {
		t.Fatal("main.version must keep a compile-time default for non-release builds")
	}
}
