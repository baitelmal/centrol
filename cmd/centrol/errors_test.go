package main

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func TestGuardInRepoWithNoCommitsSaysSo(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q")
	_, stderr, code := centrol(t, dir, "guard", "--", "true")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (user error); stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "centrol guard: this repo has no commits yet.") ||
		!strings.Contains(stderr, "Make an initial commit before running the guard") {
		t.Fatalf("missing the no-commits message; stderr:\n%s", stderr)
	}
	for _, leak := range []string{"rev-parse", "ambiguous argument", "resolving HEAD", "exit status 128"} {
		if strings.Contains(stderr, leak) {
			t.Fatalf("raw git error leaked (%q); stderr:\n%s", leak, stderr)
		}
	}
}

func TestGuardInRepoWithCommitsIsUnchanged(t *testing.T) {
	dir := initTestRepo(t)
	_, stderr, code := centrol(t, dir, "guard", "--", "sh", "-c", "exit 0")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
	if strings.Contains(stderr, "no commits yet") {
		t.Fatalf("no-commits message shown for a repo with commits:\n%s", stderr)
	}
}

func TestMissingCommandHintIsWindowsOnly(t *testing.T) {
	_, notFound := exec.LookPath("definitely-not-a-real-binary-xyz")
	wrapped := fmt.Errorf("running agent: %w", notFound)

	win := missingCommandHint(wrapped, "windows")
	if !strings.Contains(win, "must be a real executable") || !strings.Contains(win, `cmd /c "echo hi"`) {
		t.Fatalf("windows hint missing or wrong: %q", win)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if h := missingCommandHint(wrapped, goos); h != "" {
			t.Fatalf("%s got a hint: %q", goos, h)
		}
	}
	if h := missingCommandHint(errors.New("permission denied"), "windows"); h != "" {
		t.Fatalf("hint shown for an unrelated error: %q", h)
	}
}

func TestMissingCommandEndToEnd(t *testing.T) {
	dir := initTestRepo(t)
	_, stderr, code := centrol(t, dir, "guard", "--", "definitely-not-a-real-binary-xyz")
	if code == 0 || !strings.Contains(stderr, "executable file not found") {
		t.Fatalf("expected a not-found failure, got code %d:\n%s", code, stderr)
	}
	if strings.Contains(stderr, "On Windows") != (windowsHost) {
		t.Fatalf("Windows hint presence wrong for this host:\n%s", stderr)
	}
}

func TestVersionFlagPrintsAndExitsZero(t *testing.T) {
	for _, flag := range []string{"--version", "-v"} {
		out, _, code := centrol(t, t.TempDir(), flag)
		if code != 0 || !strings.HasPrefix(out, "centrol ") || strings.Count(out, "\n") != 1 {
			t.Fatalf("%s: code=%d out=%q", flag, code, out)
		}
	}
}

func TestFormatVersion(t *testing.T) {
	if got := formatVersion(nil, false); got != "centrol (version unknown)" {
		t.Fatalf("no build info: %q", got)
	}
	bi := &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}
	if got := formatVersion(bi, true); got != "centrol (version unknown)" {
		t.Fatalf("devel with no vcs data: %q", got)
	}
	bi.Settings = []debug.BuildSetting{{Key: "vcs.revision", Value: "0123456789abcdef"}, {Key: "vcs.modified", Value: "true"}}
	if got := formatVersion(bi, true); got != "centrol 0123456789ab+modified" {
		t.Fatalf("checkout build: %q", got)
	}
	bi.Main.Version = "v0.3.0"
	if got := formatVersion(bi, true); got != "centrol v0.3.0 0123456789ab+modified" {
		t.Fatalf("tagged build: %q", got)
	}
}

var windowsHost = runtime.GOOS == "windows"
