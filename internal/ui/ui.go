// Package ui holds the small, line-based terminal presentation helpers
// shared across centrol's commands: color that disappears when piped or
// when NO_COLOR is set, Unicode glyphs that degrade to ASCII on a
// non-UTF-8 terminal, log-level filtering (--quiet/--verbose), and the
// process exit code vocabulary. No TUI — everything here renders one
// line at a time to a plain io.Writer.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// Exit codes, documented in `centrol --help`.
const (
	ExitOK           = 0
	ExitUserError    = 1 // bad args, missing/invalid config
	ExitRuntimeError = 2 // lock conflict, disk full, and other operational failures
	ExitPolicyBlock  = 3 // the action was denied by policy (hard block, or an explicit Deny)
)

// Caps is a terminal's detected presentation capabilities: whether it
// can render ANSI color and whether it can render UTF-8 glyphs. The two
// are independent — a piped, non-UTF-8 log file gets neither; a
// UTF-8 terminal with NO_COLOR=1 gets glyphs but no color; a color
// terminal with a non-UTF-8 locale gets color but ASCII glyphs.
type Caps struct {
	Color bool
	UTF8  bool
}

var (
	capsMu    sync.Mutex
	capsCache = map[*os.File]Caps{}
)

// ColorEnabled reports whether w should receive ANSI color codes: only
// when w is a real terminal AND the NO_COLOR environment variable is
// unset (https://no-color.org — respecting it is non-negotiable
// regardless of TTY status). Piped or redirected output never gets
// color even if NO_COLOR isn't set.
func ColorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// utf8Enabled reports whether w is a real terminal whose locale
// (LC_ALL, falling back to LANG) advertises UTF-8. A piped or
// redirected stream — where there is no terminal encoding to speak
// of — never gets UTF-8 glyphs, matching the same "real terminal only"
// rule ColorEnabled applies.
func utf8Enabled(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	locale := os.Getenv("LC_ALL")
	if locale == "" {
		locale = os.Getenv("LANG")
	}
	upper := strings.ToUpper(locale)
	return strings.Contains(upper, "UTF-8") || strings.Contains(upper, "UTF8")
}

// DetectCaps detects f's presentation capabilities once and memoizes
// the result keyed by the *os.File pointer, so the many print calls
// during one run each get the same answer without re-stat'ing or
// re-reading the environment on every line — "detect once at startup,
// use everywhere" applies to any real terminal stream (stdout, stderr)
// this process holds open for its lifetime.
func DetectCaps(f *os.File) Caps {
	capsMu.Lock()
	defer capsMu.Unlock()
	if c, ok := capsCache[f]; ok {
		return c
	}
	c := Caps{Color: ColorEnabled(f), UTF8: utf8Enabled(f)}
	capsCache[f] = c
	return c
}

// capsFor returns w's capabilities, using the memoized DetectCaps path
// for a real *os.File (stdout/stderr — the only writers centrol prints
// to in practice) and a cheap direct computation for anything else
// (tests passing a bytes.Buffer; never a real terminal, so there is no
// repeated-detection cost to avoid).
func capsFor(w io.Writer) Caps {
	if f, ok := w.(*os.File); ok {
		return DetectCaps(f)
	}
	return Caps{Color: ColorEnabled(w), UTF8: utf8Enabled(w)}
}

// Glyph names, used with Glyph(w, ...) — the Unicode form is what's
// passed to Glyph; it returns the ASCII fallback automatically when w's
// terminal can't render UTF-8.
const (
	GlyphWarn    = "⚠" // ⚠  out-of-scope / flagged
	GlyphObserve = "◇" // ◇  observe-mode entry (would_flag/would_block)
	GlyphOK      = "✓" // ✓  clean / allowed
	GlyphBlock   = "✗" // ✗  blocked / denied
)

var asciiFallback = map[string]string{
	GlyphWarn:    "!",
	GlyphObserve: "o",
	GlyphOK:      "+",
	GlyphBlock:   "x",
}

// Glyph returns g as-is if w's terminal supports UTF-8, or its ASCII
// fallback (per the table above) otherwise. Color and Unicode support
// are independent capabilities — a glyph can still be painted a color
// after falling back to ASCII.
func Glyph(w io.Writer, g string) string {
	if capsFor(w).UTF8 {
		return g
	}
	if ascii, ok := asciiFallback[g]; ok {
		return ascii
	}
	return g
}

const (
	codeReset  = "\033[0m"
	codeRed    = "\033[31m"
	codeYellow = "\033[33m"
	codeGreen  = "\033[32m"
)

// Painter renders colored text conditionally: every method degrades to
// plain text when color is disabled, so callers never need their own
// if-color branch.
type Painter struct {
	enabled bool
}

// NewPainter builds a Painter for w, using w's memoized capabilities
// (see DetectCaps) when w is a real *os.File.
func NewPainter(w io.Writer) Painter {
	return Painter{enabled: capsFor(w).Color}
}

func (p Painter) paint(code, s string) string {
	if !p.enabled {
		return s
	}
	return code + s + codeReset
}

// Red is for errors and hard blocks.
func (p Painter) Red(s string) string { return p.paint(codeRed, s) }

// Yellow is for warnings and flags.
func (p Painter) Yellow(s string) string { return p.paint(codeYellow, s) }

// Green is for success/clean states.
func (p Painter) Green(s string) string { return p.paint(codeGreen, s) }

// Errorf writes a red "error: <message>" line to w, followed by one or
// more indented "next command" hint lines — every error in this codebase
// names what to run next rather than stopping at "what went wrong".
func Errorf(w io.Writer, hints []string, format string, args ...interface{}) {
	p := NewPainter(w)
	fmt.Fprintf(w, "%s\n", p.Red("error: "+fmt.Sprintf(format, args...)))
	for _, h := range hints {
		fmt.Fprintf(w, "  %s\n", h)
	}
}

// Level is the three-value log_level vocabulary (info < warn < debug,
// in verbosity — "warn" hides more than "info", "debug" hides nothing).
type Level int

const (
	LevelWarn  Level = iota // --quiet: warnings, errors, and the run summary only
	LevelInfo               // default: step boundaries, prompts, errors, the run summary
	LevelDebug              // --verbose: everything, including internal state transitions
)

// ParseLevel maps a resolved general.log_level string ("info" | "warn" |
// "debug") to a Level. Callers that already validated the string via
// policy.ResolveLogLevel can ignore the ok=false case; it exists so a
// bad value fails loudly rather than silently defaulting.
func ParseLevel(s string) (Level, bool) {
	switch s {
	case "warn":
		return LevelWarn, true
	case "info":
		return LevelInfo, true
	case "debug":
		return LevelDebug, true
	default:
		return LevelInfo, false
	}
}

// Logger gates output by level: Info-level lines are dropped entirely
// under --quiet, Debug-level lines print only under --verbose, and
// Warn/Error always print. The run summary and the final exit message
// are printed directly with fmt, never through a Logger, so they are
// never subject to this filtering (per spec: --quiet suppresses
// info-level output but not the summary or the exit code message).
type Logger struct {
	w     io.Writer
	level Level
}

// NewLogger builds a Logger writing to w at the given level.
func NewLogger(w io.Writer, level Level) Logger {
	return Logger{w: w, level: level}
}

// Debugf prints only when the logger's level is LevelDebug (--verbose).
// Intended for internal state transitions a normal run never needs to
// see.
func (l Logger) Debugf(format string, args ...interface{}) {
	if l.level < LevelDebug {
		return
	}
	fmt.Fprintf(l.w, format, args...)
}

// Infof prints at LevelInfo or LevelDebug, and is suppressed under
// --quiet (LevelWarn). Intended for step boundaries and prompts — the
// per-step "Snapshotting repo... ✓" lines, not errors or the summary.
func (l Logger) Infof(format string, args ...interface{}) {
	if l.level < LevelInfo {
		return
	}
	fmt.Fprintf(l.w, format, args...)
}

// Warnf always prints, regardless of level — warnings and errors are
// never suppressed by --quiet.
func (l Logger) Warnf(format string, args ...interface{}) {
	fmt.Fprintf(l.w, format, args...)
}
