package ui

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestColorDisabledForNonTTYWriter(t *testing.T) {
	var buf bytes.Buffer
	if ColorEnabled(&buf) {
		t.Fatalf("expected color disabled for a plain bytes.Buffer (not a *os.File)")
	}
}

func TestColorDisabledWhenNoColorSet(t *testing.T) {
	os.Setenv("NO_COLOR", "1")
	defer os.Unsetenv("NO_COLOR")
	if ColorEnabled(os.Stdout) {
		t.Fatalf("expected NO_COLOR to disable color even for a real *os.File")
	}
}

func TestPainterDegradesToPlainTextWhenDisabled(t *testing.T) {
	var buf bytes.Buffer
	p := NewPainter(&buf) // buf is never a TTY, so this is always disabled
	if got := p.Red("boom"); got != "boom" {
		t.Fatalf("expected plain text with color disabled, got %q", got)
	}
	if strings.Contains(p.Yellow("warn"), "\033[") {
		t.Fatalf("expected no ANSI codes with color disabled")
	}
}

func TestErrorfIncludesNextCommandHints(t *testing.T) {
	var buf bytes.Buffer
	Errorf(&buf, []string{
		`run "centrol guard -- <agent>" to start a session`,
		`or "centrol scope +<path>" to amend the current one`,
	}, "no active contract for this run")

	out := buf.String()
	if !strings.Contains(out, "error: no active contract for this run") {
		t.Fatalf("expected the error message itself, got %q", out)
	}
	if !strings.Contains(out, "centrol guard -- <agent>") || !strings.Contains(out, "centrol scope +<path>") {
		t.Fatalf("expected both next-command hints present, got %q", out)
	}
}

func TestErrorfWithNoHintsStillWritesTheMessage(t *testing.T) {
	var buf bytes.Buffer
	Errorf(&buf, nil, "something went wrong: %d", 42)
	if !strings.Contains(buf.String(), "something went wrong: 42") {
		t.Fatalf("expected the formatted message, got %q", buf.String())
	}
}

// Glyph falls back to ASCII for a non-*os.File writer (never a real
// terminal, so utf8Enabled is always false for it) and passes the
// Unicode glyph through unchanged only when the target genuinely
// supports it. Since capsFor only memoizes *os.File writers, a plain
// bytes.Buffer is always treated as non-UTF-8/non-color, matching how
// a piped or redirected stream is treated.
func TestGlyphFallsBackToASCIIForNonTerminalWriter(t *testing.T) {
	var buf bytes.Buffer
	cases := map[string]string{
		GlyphWarn:    "!",
		GlyphObserve: "o",
		GlyphOK:      "+",
		GlyphBlock:   "x",
	}
	for glyph, ascii := range cases {
		if got := Glyph(&buf, glyph); got != ascii {
			t.Fatalf("Glyph(%q) = %q, want ASCII fallback %q", glyph, got, ascii)
		}
	}
}

func TestDetectCapsMemoizesPerFile(t *testing.T) {
	// Detecting the same *os.File twice must not need to re-stat or
	// re-read the environment differently between calls: the second
	// call must return the exact same struct value, proving it came
	// from the cache rather than a fresh (and possibly different, if
	// the environment changed mid-run) detection.
	os.Setenv("NO_COLOR", "1")
	c1 := DetectCaps(os.Stderr)
	os.Unsetenv("NO_COLOR")
	c2 := DetectCaps(os.Stderr)
	if c1 != c2 {
		t.Fatalf("expected memoized caps to stay constant across calls even after the environment changed: %+v vs %+v", c1, c2)
	}
}

func TestLoggerLevelsGateOutput(t *testing.T) {
	var buf bytes.Buffer
	quiet := NewLogger(&buf, LevelWarn)
	quiet.Infof("info line\n")
	quiet.Debugf("debug line\n")
	quiet.Warnf("warn line\n")
	out := buf.String()
	if strings.Contains(out, "info line") || strings.Contains(out, "debug line") {
		t.Fatalf("expected --quiet (LevelWarn) to suppress info/debug lines, got %q", out)
	}
	if !strings.Contains(out, "warn line") {
		t.Fatalf("expected warn line to always print, got %q", out)
	}

	buf.Reset()
	verbose := NewLogger(&buf, LevelDebug)
	verbose.Infof("info2\n")
	verbose.Debugf("debug2\n")
	out = buf.String()
	if !strings.Contains(out, "info2") || !strings.Contains(out, "debug2") {
		t.Fatalf("expected --verbose (LevelDebug) to print everything, got %q", out)
	}
}

func TestParseLevel(t *testing.T) {
	if l, ok := ParseLevel("warn"); !ok || l != LevelWarn {
		t.Fatalf("ParseLevel(warn) = %v, %v", l, ok)
	}
	if l, ok := ParseLevel("info"); !ok || l != LevelInfo {
		t.Fatalf("ParseLevel(info) = %v, %v", l, ok)
	}
	if l, ok := ParseLevel("debug"); !ok || l != LevelDebug {
		t.Fatalf("ParseLevel(debug) = %v, %v", l, ok)
	}
	if _, ok := ParseLevel("bogus"); ok {
		t.Fatalf("expected ParseLevel(bogus) to report ok=false")
	}
}
