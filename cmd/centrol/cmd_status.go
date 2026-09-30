package main

import (
	"fmt"
	"os"
	"time"

	"github.com/scirem/centrol/internal/session"
	"github.com/scirem/centrol/internal/ui"
)

// cmdStatus implements `centrol status`: active run + contract + the
// System Honesty Score, computed fresh from the ledger every time (see
// internal/session.HonestyScore) rather than stored anywhere. Fits on
// one screen, six lines maximum, ending with a suggested next command.
func cmdStatus(args []string) {
	root, err := repoRoot()
	if err != nil {
		fatalf("centrol status: %v", err)
	}

	marker, active, err := session.ReadCurrentRun(centrolDir(root))
	if err != nil {
		fatalf("centrol status: %v", err)
	}

	g, err := openGovernorAt(root)
	if err != nil {
		fatalf("centrol status: %v", err)
	}
	const window = 500
	entries, err := g.Tail(window)
	if err != nil {
		fatalf("centrol status: %v", err)
	}
	score := session.HonestyScore(entries)

	verifyRes, verifyErr := g.Verify()
	chainState := "unverified"
	if verifyErr == nil {
		if verifyRes.OK {
			chainState = "chain verified"
		} else {
			chainState = "CHAIN BROKEN"
		}
	}

	var flagged, blocked, silenced int
	for _, e := range entries {
		switch e.Type {
		case "policy.violation":
			flagged++
		case "tool.blocked":
			blocked++
		case "policy.silence":
			silenced++
		}
	}

	p := ui.NewPainter(os.Stdout)

	fmt.Println("Centrol status")
	if active {
		elapsed := time.Since(marker.StartedAt).Round(time.Second)
		fmt.Printf("  Run:      %s (active, %s)\n", marker.RunID, elapsed)
	} else {
		fmt.Println("  Run:      (none)")
	}
	if len(marker.AllowedPaths) > 0 {
		fmt.Printf("  Contract: %d path(s) in scope\n", len(marker.AllowedPaths))
	} else {
		fmt.Println("  Contract: (none)")
	}
	fmt.Printf("  Ledger:   %d entries (v1, %s)\n", verifyRes.EntryCount, chainState)
	scoreStr := fmt.Sprintf("%d", score)
	switch {
	case score < 60:
		scoreStr = p.Red(scoreStr)
	case score < 90:
		scoreStr = p.Yellow(scoreStr)
	default:
		scoreStr = p.Green(scoreStr)
	}
	fmt.Printf("  SHS:      %s\n", scoreStr)
	fmt.Printf("  Events:   %d flagged, %d blocked, %d silenced\n", flagged, blocked, silenced)

	next := "centrol audit"
	if !active {
		next = "centrol guard -- <agent>"
	}
	fmt.Printf("  Next:     %s\n", next)
}
