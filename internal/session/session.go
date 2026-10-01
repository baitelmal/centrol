// Package session holds the small pieces of cross-process, cross-command
// state the CLI needs that don't belong in the ledger itself: which run
// (if any) is currently active, a way for `centrol scope` to reach a
// live run's in-memory contract, and the honesty score computation.
//
// None of this touches the watcher's .centrol//.git exclusion logic —
// that stays exactly as guard implements it. Scope amendments reach a
// live run through a dedicated poll file instead, precisely so nothing
// here needs to loosen that hard exclusion.
package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/scirem/centrol/internal/ledger"
)

const (
	currentRunFile    = "current-run.json"
	scopeRequestsFile = "scope-requests.jsonl"
)

// RunMarker records which run (guard or proxy) is currently active, for
// `centrol status` and so a bare `centrol undo` knows which run's
// snapshot to restore by default.
type RunMarker struct {
	RunID        string    `json:"run_id"`
	Kind         string    `json:"kind"` // "guard" or "proxy"
	RepoRoot     string    `json:"repo_root,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	SnapshotDir  string    `json:"snapshot_dir,omitempty"`
	AllowedPaths []string  `json:"allowed_paths,omitempty"`
}

// WriteCurrentRun records the active run marker. Overwrites any
// previous marker — only one run is "current" at a time in this
// single-binary, no-daemon design.
func WriteCurrentRun(centrolDir string, m RunMarker) error {
	// 0700/0600: everything under .centrol/ is private to the repo
	// owner — RunMarker includes RepoRoot and AllowedPaths, which are
	// not secrets themselves, but the directory-wide bar (see ledger.Open
	// and AppendScopeRequest below, same rule) is simplest kept uniform.
	if err := os.MkdirAll(centrolDir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(centrolDir, currentRunFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(centrolDir, currentRunFile))
}

// ReadCurrentRun returns the active run marker, if any.
func ReadCurrentRun(centrolDir string) (RunMarker, bool, error) {
	b, err := os.ReadFile(filepath.Join(centrolDir, currentRunFile))
	if err != nil {
		if os.IsNotExist(err) {
			return RunMarker{}, false, nil
		}
		return RunMarker{}, false, err
	}
	var m RunMarker
	if err := json.Unmarshal(b, &m); err != nil {
		return RunMarker{}, false, err
	}
	return m, true, nil
}

// ClearCurrentRun removes the marker at run end. Best-effort: a missing
// marker is not an error (the run may have crashed before writing one,
// or another process already cleared it).
func ClearCurrentRun(centrolDir string) error {
	err := os.Remove(filepath.Join(centrolDir, currentRunFile))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ScopeRequest is one `centrol scope +<path>` amendment.
type ScopeRequest struct {
	Path string    `json:"path"`
	TS   time.Time `json:"ts"`
}

// AppendScopeRequest records a scope amendment request. This file lives
// under .centrol/, which the fsnotify watcher hard-excludes — a live
// run notices new requests via PollScopeRequests, not the watcher.
func AppendScopeRequest(centrolDir, path string) error {
	if err := os.MkdirAll(centrolDir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(ScopeRequest{Path: path, TS: time.Now().UTC()})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(centrolDir, scopeRequestsFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// PollScopeRequests reads scope requests appended since fromOffset
// (bytes into the file) and returns them plus the new offset to poll
// from next time. Safe to call on a file that doesn't exist yet (no
// `centrol scope` call has happened this run) — returns no requests and
// offset 0.
func PollScopeRequests(centrolDir string, fromOffset int64) ([]ScopeRequest, int64, error) {
	path := filepath.Join(centrolDir, scopeRequestsFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fromOffset, nil
		}
		return nil, fromOffset, err
	}
	defer f.Close()

	if _, err := f.Seek(fromOffset, os.SEEK_SET); err != nil {
		return nil, fromOffset, err
	}

	var out []ScopeRequest
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	consumed := fromOffset
	for scanner.Scan() {
		line := scanner.Bytes()
		consumed += int64(len(line)) + 1 // +1 for the newline the scanner stripped
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var req ScopeRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue // a malformed line here is not agent-facing; skip rather than fail the whole poll
		}
		out = append(out, req)
	}
	if err := scanner.Err(); err != nil {
		return out, fromOffset, err
	}
	return out, consumed, nil
}

// HonestyScore is the System Honesty Score: derived from the ledger on
// read, never asserted or stored. Starts at 100, deducts a weighted
// amount per policy.violation and policy.silence in the given window,
// and recovers slowly (a small amount per clean entry) — so a single
// bad run dents the score, but it climbs back with continued clean
// activity rather than staying dented forever or snapping back
// instantly.
func HonestyScore(entries []ledger.Entry) int {
	const (
		violationPenalty = 6.0
		silencePenalty   = 3.0
		cleanRecovery    = 0.4
		floor            = 0.0
		ceiling          = 100.0
	)
	score := ceiling
	for _, e := range entries {
		switch e.Type {
		case "policy.violation", "tool.blocked":
			score -= violationPenalty
		case "policy.silence":
			score -= silencePenalty
		default:
			score += cleanRecovery
		}
		if score < floor {
			score = floor
		}
		if score > ceiling {
			score = ceiling
		}
	}
	return int(score)
}
