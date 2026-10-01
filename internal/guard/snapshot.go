package guard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ManifestEntry is one file's record in manifest.json.
type ManifestEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime string `json:"mtime"`
	SHA256  string `json:"sha256"`
	Tracked bool   `json:"tracked"`
}

// Manifest is the full file inventory captured at snapshot time.
type Manifest struct {
	RunID string          `json:"run_id"`
	Files []ManifestEntry `json:"files"`
}

// runGit returns stdout only, trimmed. Stderr is captured separately so
// warnings/notices on stderr (e.g. line-ending notes) never leak into
// stdout and corrupt output we treat as data, such as diff patches.
// isCentrolOrGitPath mirrors the watcher's hard .centrol//.git exclusion
// (see hardExcludedAbs in watch.go) for the repo-relative paths this
// file works with. It is intentionally a separate implementation
// rather than a refactor of the watcher's own logic — the watcher's
// exclusion is a mandatory, untouched invariant on its own; this is a
// second application of the same rule to snapshot/restore, which walks
// git's own file lists rather than fsnotify events and so needs its own
// unconditional check before anything else (including .centrolignore)
// gets a say. Without this, a snapshot recursively captures its own
// ledger and snapshot directories into themselves.
func isCentrolOrGitPath(relPath string) bool {
	relPath = filepath.ToSlash(relPath)
	return relPath == ".centrol" || strings.HasPrefix(relPath, ".centrol/") ||
		relPath == ".git" || strings.HasPrefix(relPath, ".git/")
}

func runGit(repoRoot string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoRoot
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// runGitRaw is like runGit but returns stdout untrimmed, for output
// where leading/trailing whitespace is semantically meaningful (diff
// patches — a trailing newline can matter to `git apply`).
func runGitRaw(repoRoot string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoRoot
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// Snapshot writes the four required components under snapshotDir:
// head.txt, stash.diff, untracked/, manifest.json. It never mutates the
// repo (git stash create does not touch the working tree or the stash
// ref list, per the mandatory instruction to avoid relying on stash
// refs).
func Snapshot(repoRoot, snapshotDir string, ignore *IgnoreSet) error {
	// 0700/0600 throughout this function: a snapshot captures the full
	// working-tree diff and every untracked file's content (see the
	// untracked-file loop below), which routinely includes secrets an
	// agent was mid-edit on — none of .centrol/snapshots/ may be
	// world-readable.
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		return err
	}

	head, err := runGit(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("snapshot: resolving HEAD: %w", err)
	}
	if err := os.WriteFile(filepath.Join(snapshotDir, "head.txt"), []byte(head+"\n"), 0o600); err != nil {
		return err
	}

	// git stash create returns a commit hash representing working tree +
	// index changes without touching the working tree or the stash ref
	// list; empty output means a clean tree.
	stashHash, err := runGit(repoRoot, "stash", "create")
	if err != nil {
		return fmt.Errorf("snapshot: git stash create: %w", err)
	}
	diff := ""
	if stashHash != "" {
		diff, err = runGitRaw(repoRoot, "diff", "--binary", head, stashHash)
		if err != nil {
			return fmt.Errorf("snapshot: diffing stash: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(snapshotDir, "stash.diff"), []byte(diff), 0o600); err != nil {
		return err
	}

	untrackedOut, err := runGit(repoRoot, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return fmt.Errorf("snapshot: listing untracked files: %w", err)
	}
	trackedOut, err := runGit(repoRoot, "ls-files")
	if err != nil {
		return fmt.Errorf("snapshot: listing tracked files: %w", err)
	}

	untrackedDir := filepath.Join(snapshotDir, "untracked")
	if err := os.MkdirAll(untrackedDir, 0o700); err != nil {
		return err
	}

	manifest := Manifest{RunID: filepath.Base(snapshotDir)}

	// addManifestEntry reports whether it actually recorded relPath
	// (added), distinct from an error. Audit fix: the caller below used
	// to treat addManifestEntry's "skip" return (nil error) as
	// indistinguishable from "added," then unconditionally copyFile'd
	// the untracked entry regardless. copyFile's os.Open follows
	// symlinks, so an untracked symlink committed into the race window
	// (e.g. one pointing at ~/.ssh/id_rsa) had its TARGET's content
	// silently captured into the snapshot store on every guarded run —
	// exactly the kind of disclosure the symlink check here was already
	// trying to prevent for the manifest entry itself, just not for the
	// copy. The added bool lets the untracked loop skip the copy for
	// anything this function skipped, for the same reasons (symlink,
	// directory, or vanished between listing and stat).
	addManifestEntry := func(relPath string, tracked bool) (added bool, err error) {
		full := filepath.Join(repoRoot, relPath)
		info, err := os.Lstat(full)
		if err != nil {
			return false, nil // file vanished between listing and stat; skip
		}
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
			return false, nil
		}
		sum, err := sha256File(full)
		if err != nil {
			return false, err
		}
		manifest.Files = append(manifest.Files, ManifestEntry{
			Path: filepath.ToSlash(relPath), Size: info.Size(),
			ModTime: info.ModTime().Format("2006-01-02T15:04:05.000Z07:00"),
			SHA256:  sum, Tracked: tracked,
		})
		return true, nil
	}

	for _, line := range splitNonEmpty(trackedOut) {
		if isCentrolOrGitPath(line) || ignore.Match(line) {
			continue
		}
		if _, err := addManifestEntry(line, true); err != nil {
			return err
		}
	}
	for _, line := range splitNonEmpty(untrackedOut) {
		if isCentrolOrGitPath(line) || ignore.Match(line) {
			continue
		}
		added, err := addManifestEntry(line, false)
		if err != nil {
			return err
		}
		if !added {
			continue // symlink, directory, or vanished — never copy; see addManifestEntry's doc comment
		}
		src := filepath.Join(repoRoot, line)
		dst := filepath.Join(untrackedDir, line)
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("snapshot: copying untracked %s: %w", line, err)
		}
		if err := os.Chmod(dst, 0o600); err != nil {
			return fmt.Errorf("snapshot: restricting permissions on untracked %s: %w", line, err)
		}
	}

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(snapshotDir, "manifest.json"), manifestBytes, 0o600)
}

// RestoreOutcome reports what a Restore could and couldn't do, per the
// honest-scope contract: it restores tracked git state + untracked
// files present at snapshot time, but does not restore ignored files,
// databases, external state, or running processes/open handles, and it
// detects and logs out-of-scope changes it cannot restore.
type RestoreOutcome struct {
	RestoredTracked   bool
	RestoredUntracked []string
	OutOfScope        []string // new untracked files present now that weren't in the snapshot and aren't restorable from it
}

// Restore reverts repoRoot to the state captured in snapshotDir. Callers
// MUST take a pre-undo snapshot of the current state before calling
// Restore (see PreUndoSnapshot) so a failed rollback is itself
// recoverable.
func Restore(repoRoot, snapshotDir string) (RestoreOutcome, error) {
	var out RestoreOutcome

	headBytes, err := os.ReadFile(filepath.Join(snapshotDir, "head.txt"))
	if err != nil {
		return out, fmt.Errorf("restore: reading head.txt: %w", err)
	}
	head := strings.TrimSpace(string(headBytes))

	if _, err := runGit(repoRoot, "reset", "--hard", head); err != nil {
		return out, fmt.Errorf("restore: git reset --hard %s: %w", head, err)
	}
	out.RestoredTracked = true

	diffBytes, err := os.ReadFile(filepath.Join(snapshotDir, "stash.diff"))
	if err != nil {
		return out, fmt.Errorf("restore: reading stash.diff: %w", err)
	}
	if strings.TrimSpace(string(diffBytes)) != "" {
		tmp, err := os.CreateTemp("", "centrol-restore-*.diff")
		if err != nil {
			return out, err
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.Write(diffBytes); err != nil {
			tmp.Close()
			return out, err
		}
		tmp.Close()
		if _, err := runGit(repoRoot, "apply", "--binary", tmp.Name()); err != nil {
			return out, fmt.Errorf("restore: git apply stash.diff: %w", err)
		}
	}

	untrackedDir := filepath.Join(snapshotDir, "untracked")
	if info, err := os.Stat(untrackedDir); err == nil && info.IsDir() {
		err := filepath.Walk(untrackedDir, func(path string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return err
			}
			rel, err := filepath.Rel(untrackedDir, path)
			if err != nil {
				return err
			}
			dst := filepath.Join(repoRoot, rel)
			if err := copyFile(path, dst); err != nil {
				return fmt.Errorf("restoring untracked %s: %w", rel, err)
			}
			out.RestoredUntracked = append(out.RestoredUntracked, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return out, err
		}
	}

	// Detect out-of-scope changes: untracked files present now that were
	// not part of the snapshot and so could not have been restored from
	// it (e.g. ignored files, files the agent created after the snapshot
	// that the snapshot's own untracked/ copy never captured).
	nowUntracked, err := runGit(repoRoot, "ls-files", "--others", "--exclude-standard")
	if err == nil {
		restoredSet := map[string]bool{}
		for _, r := range out.RestoredUntracked {
			restoredSet[r] = true
		}
		for _, line := range splitNonEmpty(nowUntracked) {
			if isCentrolOrGitPath(line) {
				continue
			}
			if !restoredSet[filepath.ToSlash(line)] {
				out.OutOfScope = append(out.OutOfScope, line)
			}
		}
	}

	return out, nil
}

// PreUndoSnapshot is a thin wrapper making the "always snapshot before a
// destructive rollback" rule impossible to accidentally skip at a call
// site: it names the snapshot directory per the <run_id>.pre-undo
// convention documented for `centrol undo --from`.
func PreUndoSnapshot(repoRoot, snapshotsRoot, runID string, ignore *IgnoreSet) (string, error) {
	dir := filepath.Join(snapshotsRoot, runID+".pre-undo")
	if err := Snapshot(repoRoot, dir, ignore); err != nil {
		return "", fmt.Errorf("pre-undo snapshot failed, refusing to proceed with rollback: %w", err)
	}
	return dir, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func splitNonEmpty(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	var out []string
	for _, l := range lines {
		l = strings.TrimRight(l, "\r")
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
