package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/scirem/centrol/internal/ledger"
	"github.com/scirem/centrol/internal/ui"
)

// cmdAudit implements `centrol audit` and `centrol audit --verify`.
// Default view: last 20 entries, newest first, one line per event, with
// --flagged/--blocked/--src/--run/--since filters.
func cmdAudit(args []string) {
	verify := false
	flaggedOnly := false
	blockedOnly := false
	srcFilter := ""
	runFilter := ""
	since := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--verify":
			verify = true
		case args[i] == "--flagged":
			flaggedOnly = true
		case args[i] == "--blocked":
			blockedOnly = true
		case args[i] == "--src" && i+1 < len(args):
			srcFilter = args[i+1]
			i++
		case args[i] == "--run" && i+1 < len(args):
			runFilter = args[i+1]
			i++
		case args[i] == "--since" && i+1 < len(args):
			since = args[i+1]
			i++
		}
	}

	root, err := repoRoot()
	if err != nil {
		fatalf("centrol audit: %v", err)
	}
	g, err := openGovernorAt(root)
	if err != nil {
		fatalf("centrol audit: %v", err)
	}

	if verify {
		res, err := g.Verify()
		if err != nil {
			fatalf("centrol audit --verify: %v", err)
		}
		p := ui.NewPainter(os.Stdout)
		if !res.OK {
			fmt.Println(p.Red("FAILED: chain integrity broken"))
			fmt.Printf("  segment: %s\n  seq:     %d\n  reason:  %s\n",
				res.FailedAt.Segment, res.FailedAt.Seq, res.FailedAt.Reason)
			os.Exit(ui.ExitRuntimeError)
		}
		fmt.Println(p.Green(fmt.Sprintf("OK: %d entries across %d segment(s), chain intact", res.EntryCount, res.SegmentCount)))
		return
	}

	entries, err := g.Tail(0)
	if err != nil {
		fatalf("centrol audit: %v", err)
	}

	var sinceTime time.Time
	if since != "" {
		d, err := parseSince(since)
		if err != nil {
			fatalHint(ui.ExitUserError, []string{`--since accepts a number followed by m/h/d, e.g. "30m", "1h", "2d"`}, "%v", err)
		}
		sinceTime = time.Now().Add(-d)
	}

	var filtered []ledger.Entry
	for _, e := range entries {
		if flaggedOnly && e.Type != "policy.violation" {
			continue
		}
		if blockedOnly && e.Type != "tool.blocked" {
			continue
		}
		if srcFilter != "" && e.Src != srcFilter {
			continue
		}
		if runFilter != "" && e.Run != runFilter {
			continue
		}
		if !sinceTime.IsZero() {
			ts, err := time.Parse(time.RFC3339Nano, e.TS)
			if err == nil && ts.Before(sinceTime) {
				continue
			}
		}
		filtered = append(filtered, e)
	}

	if len(filtered) == 0 {
		fmt.Println("(no matching entries)")
		return
	}

	// Newest first; default to the last 20 unless a filter was given
	// (filters imply the person wants the matching set, not just a
	// recent window of it).
	limit := 20
	anyFilter := flaggedOnly || blockedOnly || srcFilter != "" || runFilter != "" || since != ""
	start := 0
	if !anyFilter && len(filtered) > limit {
		start = len(filtered) - limit
	}
	window := filtered[start:]

	p := ui.NewPainter(os.Stdout)
	for i := len(window) - 1; i >= 0; i-- {
		printAuditLine(p, window[i])
	}
}

func printAuditLine(p ui.Painter, e ledger.Entry) {
	ts, err := time.Parse(time.RFC3339Nano, e.TS)
	tsStr := e.TS
	if err == nil {
		tsStr = ts.Local().Format("15:04:05")
	}

	symbol := ui.Glyph(os.Stdout, ui.GlyphOK)
	switch e.Type {
	case "policy.violation":
		symbol = p.Yellow(ui.Glyph(os.Stdout, ui.GlyphWarn))
	case "tool.blocked":
		symbol = p.Red(ui.Glyph(os.Stdout, ui.GlyphBlock))
	case "policy.silence":
		symbol = p.Yellow(ui.Glyph(os.Stdout, ui.GlyphObserve)) // reused for observe-mode would_flag/would_block below
	}
	if decision, ok := payloadString(e.Payload, "decision"); ok && (decision == "would_flag" || decision == "would_block") {
		symbol = ui.Glyph(os.Stdout, ui.GlyphObserve) // observe-mode: visually distinct from an enforced ✓/⚠/✗
	}

	detail := auditDetail(e)
	fmt.Printf("%s  %-6s  %-16s  %-24s  %s\n", tsStr, e.Src, e.Type, detail, symbol)
}

// auditDetail renders the single most useful field from an entry's
// payload for the one-line view (tool name, path, run id — whichever
// this event type carries), falling back to the run id.
func auditDetail(e ledger.Entry) string {
	for _, key := range []string{"tool", "path", "server", "amend"} {
		if v, ok := payloadString(e.Payload, key); ok {
			return v
		}
	}
	return "run=" + e.Run
}

func payloadString(raw json.RawMessage, key string) (string, bool) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", false
	}
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func parseSince(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty --since value")
	}
	unit := s[len(s)-1]
	numPart := s[:len(s)-1]
	n, err := strconv.Atoi(numPart)
	if err != nil {
		return 0, fmt.Errorf("--since %q: not a number followed by m/h/d", s)
	}
	switch unit {
	case 'm':
		return time.Duration(n) * time.Minute, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("--since %q: unit must be m, h, or d", s)
	}
}
