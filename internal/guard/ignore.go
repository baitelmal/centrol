package guard

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// DefaultIgnorePatterns ships as the default .centrolignore template and is
// also used as the in-memory default when no .centrolignore file exists.
var DefaultIgnorePatterns = []string{
	"node_modules",
	".venv",
	"dist",
	"build",
	".next",
	"__pycache__",
}

// IgnoreSet is a simple, dependency-free ignore matcher: each pattern is
// either a plain directory/file name matched against any path component,
// or a shell glob (via path/filepath.Match) matched against the full
// repo-relative path. This is intentionally not full gitignore syntax —
// centrol's ignore file only needs to keep noisy build directories out of
// snapshots and the watcher, not replicate git.
type IgnoreSet struct {
	patterns []string
}

// LoadIgnore reads .centrolignore from repoRoot if present, and always
// includes DefaultIgnorePatterns regardless (the defaults are a floor,
// not something the user can accidentally unset — the watcher's own
// .centrol/ and .git/ exclusions below are unconditional in a separate,
// higher-priority path).
func LoadIgnore(repoRoot string) (*IgnoreSet, error) {
	set := &IgnoreSet{patterns: append([]string{}, DefaultIgnorePatterns...)}
	f, err := os.Open(filepath.Join(repoRoot, ".centrolignore"))
	if err != nil {
		if os.IsNotExist(err) {
			return set, nil
		}
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		set.patterns = append(set.patterns, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return set, nil
}

// Match reports whether relPath (repo-relative, forward-slash) should be
// ignored.
func (s *IgnoreSet) Match(relPath string) bool {
	relPath = filepath.ToSlash(relPath)
	parts := strings.Split(relPath, "/")
	for _, pat := range s.patterns {
		pat = filepath.ToSlash(pat)
		// Plain component match: pattern with no glob/slash matches any
		// path component (e.g. "node_modules" matches "a/node_modules/b").
		if !strings.ContainsAny(pat, "*?[/") {
			for _, part := range parts {
				if part == pat {
					return true
				}
			}
			continue
		}
		if ok, _ := filepath.Match(pat, relPath); ok {
			return true
		}
		if strings.HasPrefix(relPath, strings.TrimSuffix(pat, "/")+"/") {
			return true
		}
	}
	return false
}
