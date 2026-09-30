package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/scirem/centrol/internal/guard"
)

// cmdUndo implements `centrol undo`, `centrol undo --list`,
// `centrol undo --from <snapshot_id>`, and `centrol undo <run_id>`.
func cmdUndo(args []string) {
	root, err := repoRoot()
	if err != nil {
		fatalf("centrol undo: %v", err)
	}
	snapRoot := snapshotsDir(root)

	list := false
	from := ""
	runID := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--list":
			list = true
		case args[i] == "--from" && i+1 < len(args):
			from = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--from="):
			from = strings.TrimPrefix(args[i], "--from=")
		case !strings.HasPrefix(args[i], "-"):
			runID = args[i]
		}
	}

	if list {
		listSnapshots(snapRoot)
		return
	}

	var snapshotName string
	switch {
	case from != "":
		snapshotName = from
	case runID != "":
		snapshotName = runID
	default:
		latest, err := mostRecentSnapshot(snapRoot)
		if err != nil {
			fatalf("centrol undo: %v", err)
		}
		snapshotName = latest
	}

	snapDir := filepath.Join(snapRoot, snapshotName)
	if _, err := os.Stat(snapDir); err != nil {
		fatalf("centrol undo: no snapshot named %q (see `centrol undo --list`)", snapshotName)
	}

	g, err := openGovernorAt(root)
	if err != nil {
		fatalf("centrol undo: %v", err)
	}
	emit := emitFunc(g)

	ignore, err := guard.LoadIgnore(root)
	if err != nil {
		fatalf("centrol undo: %v", err)
	}

	// MANDATORY: snapshot the current (pre-rollback) state before doing
	// anything destructive, so a rollback that fails partway is itself
	// recoverable via `centrol undo --from <snapshot_id>.pre-undo`.
	preUndoRunID := snapshotName
	preUndoDir, err := guard.PreUndoSnapshot(root, snapRoot, preUndoRunID, ignore)
	if err != nil {
		fatalf("centrol undo: %v", err)
	}
	fmt.Fprintf(os.Stderr, "centrol: pre-undo snapshot saved to %s\n", preUndoDir)

	outcome, err := guard.Restore(root, snapDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "centrol undo: rollback failed partway: %v\n", err)
		fmt.Fprintf(os.Stderr, "centrol: your tree may be in a mixed state. Recover with: centrol undo --from %s\n", filepath.Base(preUndoDir))
		_ = emit(preUndoRunID, "guard", "run.rollback", map[string]interface{}{
			"snapshot": snapshotName, "error": err.Error(), "pre_undo_snapshot": filepath.Base(preUndoDir),
		})
		os.Exit(1)
	}

	_ = emit(preUndoRunID, "guard", "run.rollback", map[string]interface{}{
		"snapshot": snapshotName, "restored_tracked": outcome.RestoredTracked,
		"restored_untracked_count": len(outcome.RestoredUntracked), "out_of_scope": outcome.OutOfScope,
	})

	fmt.Printf("Restored from snapshot %s.\n", snapshotName)
	fmt.Printf("  tracked git state: restored\n")
	fmt.Printf("  untracked files restored: %d\n", len(outcome.RestoredUntracked))
	if len(outcome.OutOfScope) > 0 {
		fmt.Printf("  could NOT restore (created after the snapshot, not captured by it):\n")
		for _, f := range outcome.OutOfScope {
			fmt.Printf("    - %s\n", f)
		}
	}
}

func listSnapshots(snapRoot string) {
	entries, err := os.ReadDir(snapRoot)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("(no snapshots yet — run `centrol guard -- <agent command>` first)")
			return
		}
		fatalf("centrol undo --list: %v", err)
	}
	names := dirNames(entries)
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Println("(no snapshots yet)")
		return
	}
	for _, n := range names {
		fmt.Println(n)
	}
}

func mostRecentSnapshot(snapRoot string) (string, error) {
	entries, err := os.ReadDir(snapRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no snapshots yet — run `centrol guard -- <agent command>` first")
		}
		return "", err
	}
	var candidates []string
	for _, n := range dirNames(entries) {
		if !strings.HasSuffix(n, ".pre-undo") {
			candidates = append(candidates, n)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no snapshots yet — run `centrol guard -- <agent command>` first")
	}
	sort.Strings(candidates) // run IDs are timestamp-prefixed, so lexicographic == chronological
	return candidates[len(candidates)-1], nil
}

func dirNames(entries []os.DirEntry) []string {
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}
