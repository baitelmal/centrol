package main

import (
	"fmt"
	"strings"

	"github.com/scirem/centrol/internal/session"
)

// cmdScope implements `centrol scope +<path>`: it widens the active
// run's contract. Run this from a second terminal while `centrol
// guard` or `centrol proxy` is running in the first — the live run
// picks it up within half a second via its scope poller (deliberately
// not the fsnotify watcher, which hard-excludes .centrol/) and logs it
// as contract.declare.
func cmdScope(args []string) {
	if len(args) != 1 || !strings.HasPrefix(args[0], "+") {
		fatalf("centrol scope: usage: centrol scope +<path>")
	}
	path := strings.TrimPrefix(args[0], "+")
	if path == "" {
		fatalf("centrol scope: usage: centrol scope +<path>")
	}

	root, err := repoRoot()
	if err != nil {
		fatalf("centrol scope: %v", err)
	}

	marker, active, err := session.ReadCurrentRun(centrolDir(root))
	if err != nil {
		fatalf("centrol scope: %v", err)
	}
	if !active {
		fatalHint(1, []string{
			"run `centrol guard -- <agent>` to start a session",
			"or `centrol scope +<path>` to amend the current one",
		}, "no active contract for this run")
	}

	if err := session.AppendScopeRequest(centrolDir(root), path); err != nil {
		fatalf("centrol scope: %v", err)
	}

	fmt.Printf("Requested scope amendment: +%s (for run %s)\n", path, marker.RunID)
	fmt.Println("The active run will pick this up within a second and log it as contract.declare.")
}
