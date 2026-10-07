// MIT License
//
// Copyright (c) 2026 NSBaitelmal
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to
// deal in the Software without restriction, including without limitation the
// rights to use, copy, modify, merge, publish, distribute, sublicense, and/or
// sell copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING
// FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER
// DEALINGS IN THE SOFTWARE.

// centrol-verify is a standalone reference verifier for a Centrol ledger's
// hash chain.
//
// It deliberately imports nothing from the centrol module itself — not
// internal/ledger, not internal/governor, not internal/policy, nothing.
// The whole point of a reference verifier is that a third party can check
// the one guarantee that actually matters ("this ledger wasn't tampered
// with after the fact") without having to trust, or even build, the
// centrol binary that produced it. So this file re-derives the hash-chain
// rule from first principles — the entry shape is just what
// schemas/event.v1.json documents, and the hashing convention (hash every
// field except hash itself, in a fixed order, as compact encoding/json
// output) is observable from the ledger's own JSONL output — rather than
// calling into centrol's own verification code. Two independent
// implementations agreeing is worth far more than one implementation
// trusting itself.
//
// Usage:
//
//	centrol-verify <path-to-lighthouse.jsonl>
//	centrol-verify <path-to-.centrol-directory>
//
// A single file is verified as one segment. A directory is scanned for a
// ledger's segments (a base file plus any <name>.NNN<ext> rotations) and
// verified as one continuous chain across the rotation boundaries.
//
// Exit codes: 0 valid, 1 invalid (chain broken), 2 read error (bad usage,
// missing file, or a directory with no ledger segments in it).
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// entry is a ledger record, matching schemas/event.v1.json. Field names
// and JSON tags are reproduced here, independently, from that schema —
// not imported from internal/ledger.
type entry struct {
	V       int             `json:"v"`
	Seq     int             `json:"seq"`
	TS      string          `json:"ts"`
	Run     string          `json:"run"`
	Src     string          `json:"src"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
	Prev    string          `json:"prev"`
	Hash    string          `json:"hash"`
}

// entryPre is every field of entry except hash itself, in this exact
// order. This is the byte-exact shape the ledger hashes to produce
// entry.Hash: encoding/json's Marshal always emits struct fields in
// declaration order, so as long as this struct's field order matches the
// one the ledger hashed with, re-marshaling here reproduces the identical
// bytes (and Payload being json.RawMessage means its original bytes are
// carried through untouched, not reformatted).
type entryPre struct {
	V       int             `json:"v"`
	Seq     int             `json:"seq"`
	TS      string          `json:"ts"`
	Run     string          `json:"run"`
	Src     string          `json:"src"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
	Prev    string          `json:"prev"`
}

// computeHash reproduces the ledger's hash: sha256 over the compact
// JSON encoding of every field but hash, hex-encoded.
func computeHash(e entryPre) (string, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// rotatedSegmentPattern matches a rotated segment's filename, e.g.
// "lighthouse.001.jsonl" — base name, a zero-padded 3-digit index, then
// the original extension. Checked before basePattern so a rotated
// segment is never mistaken for a same-named base file.
var rotatedSegmentPattern = regexp.MustCompile(`^(.+)\.(\d{3})(\.[^.]+)$`)

// baseSegmentPattern matches a ledger's unrotated base file, e.g.
// "lighthouse.jsonl" — base name plus extension, no numeric index.
var baseSegmentPattern = regexp.MustCompile(`^(.+)(\.[^.]+)$`)

// discoverSegments resolves the CLI's one path argument to an ordered
// list of segment files to verify as one continuous chain.
//
// A path to a regular file is verified as a single segment on its own —
// centrol-verify never goes looking in that file's directory for
// siblings it wasn't asked about. A path to a directory is scanned for
// exactly one ledger's segments (a base "<name><ext>" file, if present,
// followed by any "<name>.NNN<ext>" rotations in ascending order); more
// than one distinct ledger base name in the same directory is ambiguous
// and reported as an error rather than guessed at.
func discoverSegments(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	type family struct {
		base    string
		rotated map[int]string
	}
	families := map[string]*family{}
	get := func(key string) *family {
		f := families[key]
		if f == nil {
			f = &family{rotated: map[int]string{}}
			families[key] = f
		}
		return f
	}

	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		if m := rotatedSegmentPattern.FindStringSubmatch(name); m != nil {
			idx, convErr := strconv.Atoi(m[2])
			if convErr != nil {
				continue // not actually a 3-digit index; ignore, don't guess
			}
			key := m[1] + m[3]
			get(key).rotated[idx] = filepath.Join(path, name)
			continue
		}
		if m := baseSegmentPattern.FindStringSubmatch(name); m != nil {
			if m[2] != ".jsonl" {
				continue // not a ledger segment (meta.json, lock file, etc.)
			}
			key := m[1] + m[2]
			get(key).base = filepath.Join(path, name)
			continue
		}
	}

	if len(families) == 0 {
		return nil, fmt.Errorf("no .jsonl ledger segments found in %s", path)
	}
	if len(families) > 1 {
		names := make([]string, 0, len(families))
		for k := range families {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("multiple ledger files found in %s (%s) — pass the specific file instead of the directory", path, strings.Join(names, ", "))
	}

	var fam *family
	for _, f := range families {
		fam = f
	}
	var segs []string
	if fam.base != "" {
		segs = append(segs, fam.base)
	}
	idxs := make([]int, 0, len(fam.rotated))
	for idx := range fam.rotated {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	for _, idx := range idxs {
		segs = append(segs, fam.rotated[idx])
	}
	if len(segs) == 0 {
		return nil, fmt.Errorf("no ledger segments found in %s", path)
	}
	return segs, nil
}

// result is the outcome of verifying a chain across one or more segments.
type result struct {
	ok         bool
	entryCount int
	segCount   int

	// Populated only when ok is false.
	failSeq     int // 0 when the failure has no parseable seq (a malformed line)
	hasFailSeq  bool
	failReason  string
	failEntries [][]byte // raw line(s) relevant to the failure, in file order
}

// verify walks segs in order as one continuous chain: each entry's
// stored hash must match its recomputed hash, seq must increase by
// exactly 1 across the whole chain (including across a segment
// boundary), and each entry's prev must equal the immediately preceding
// entry's hash. It stops at the first failure, mirroring "first broken
// seq + reason" rather than collecting every subsequent symptom of one
// break.
//
// A non-nil error return means a segment file itself couldn't be read
// (missing, permission denied, I/O failure) — distinct from the chain
// being invalid, which is reported in the returned result instead.
func verify(segs []string) (result, error) {
	res := result{ok: true, segCount: len(segs)}

	var prevHash string
	var prevSeq int
	var prevRaw []byte
	first := true

	for _, segPath := range segs {
		f, err := os.Open(segPath)
		if err != nil {
			return result{}, err
		}

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			raw := append([]byte(nil), scanner.Bytes()...) // Bytes() is reused by the next Scan
			if len(bytes.TrimSpace(raw)) == 0 {
				continue
			}

			var e entry
			if uerr := json.Unmarshal(raw, &e); uerr != nil {
				f.Close()
				res.ok = false
				res.failReason = fmt.Sprintf("malformed entry in %s, line %d: %v", filepath.Base(segPath), lineNo, uerr)
				res.failEntries = entriesFor(prevRaw, raw)
				return res, nil
			}
			res.entryCount++

			pre := entryPre{V: e.V, Seq: e.Seq, TS: e.TS, Run: e.Run, Src: e.Src, Type: e.Type, Payload: e.Payload, Prev: e.Prev}
			wantHash, herr := computeHash(pre)
			if herr != nil {
				f.Close()
				return result{}, fmt.Errorf("hashing entry seq %d: %w", e.Seq, herr)
			}
			if wantHash != e.Hash {
				f.Close()
				res.ok = false
				res.failSeq, res.hasFailSeq = e.Seq, true
				res.failReason = "hash mismatch (entry was tampered with)"
				res.failEntries = entriesFor(nil, raw)
				return res, nil
			}
			if !first && e.Seq != prevSeq+1 {
				f.Close()
				res.ok = false
				res.failSeq, res.hasFailSeq = e.Seq, true
				res.failReason = fmt.Sprintf("sequence gap: expected %d, got %d", prevSeq+1, e.Seq)
				res.failEntries = entriesFor(prevRaw, raw)
				return res, nil
			}
			if !first && e.Prev != prevHash {
				f.Close()
				res.ok = false
				res.failSeq, res.hasFailSeq = e.Seq, true
				res.failReason = "prev hash does not match preceding entry (chain broken)"
				res.failEntries = entriesFor(prevRaw, raw)
				return res, nil
			}

			prevHash, prevSeq, prevRaw, first = e.Hash, e.Seq, raw, false
		}
		if serr := scanner.Err(); serr != nil {
			f.Close()
			return result{}, fmt.Errorf("reading %s: %w", segPath, serr)
		}
		f.Close()
	}

	return res, nil
}

// entriesFor collects the raw line(s) relevant to a failure, in file
// order, dropping a nil prev rather than reporting it.
func entriesFor(prev, cur []byte) [][]byte {
	if prev == nil {
		return [][]byte{cur}
	}
	return [][]byte{prev, cur}
}

// run implements the CLI and returns the process exit code, so the
// tests below can exercise it directly without spawning a subprocess.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: centrol-verify <path-to-lighthouse.jsonl-or-segment-directory>")
		return 2
	}

	segs, err := discoverSegments(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "centrol-verify: %v\n", err)
		return 2
	}

	res, err := verify(segs)
	if err != nil {
		fmt.Fprintf(stderr, "centrol-verify: %v\n", err)
		return 2
	}

	if res.ok {
		fmt.Fprintf(stdout, "OK: %d entries across %d segments, chain intact\n", res.entryCount, res.segCount)
		return 0
	}

	if res.hasFailSeq {
		fmt.Fprintf(stdout, "FAIL at seq %d: %s\n", res.failSeq, res.failReason)
	} else {
		fmt.Fprintf(stdout, "FAIL: %s\n", res.failReason)
	}
	for _, e := range res.failEntries {
		fmt.Fprintf(stdout, "  %s\n", e)
	}
	return 1
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
