// Package ledger implements the append-only, hash-chained event log that
// backs centrol's accountability guarantees. The Governor is the only caller
// permitted to append; no other package should import ledger for writing.
package ledger

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
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the frozen ledger entry schema version for this release.
const SchemaVersion = 1

// DefaultRotateAtMB is the default segment size at which a new segment
// is started.
const DefaultRotateAtMB = 100

// Entry is one ledger record, matching schemas/event.v1.json.
type Entry struct {
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

// entryPre is the byte-exact shape hashed to produce Entry.Hash: every
// field except hash itself, in this fixed order. Because Payload is
// json.RawMessage, unmarshal->remarshal round-trips its original bytes
// exactly, which is what makes hash recomputation during verify stable.
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

func computeHash(e entryPre) (string, []byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), b, nil
}

// meta tracks which segment is currently open for writes. Stored as its
// own small file so segment identity survives process restarts without
// having to re-scan and re-hash every segment on startup.
type meta struct {
	CurrentIndex int `json:"current_index"` // 0 = base file, N>=1 = ".NNN" suffix
}

// Ledger is a handle to one per-repo ledger rooted at a base path such as
// ".centrol/lighthouse.jsonl". Segments are named:
//
//	lighthouse.jsonl       (index 0, first segment)
//	lighthouse.001.jsonl   (index 1, after first rotation)
//	lighthouse.002.jsonl   (index 2, ...)
type Ledger struct {
	dir           string
	name          string // "lighthouse"
	ext           string // ".jsonl"
	lockPath      string
	metaPath      string
	rotateAtBytes int64
}

// Open prepares a Ledger rooted at basePath (e.g. ".centrol/lighthouse.jsonl").
// It does not itself write anything; the directory and files are created
// lazily on first Append so that merely constructing a Ledger has no
// side effects.
func Open(basePath string, rotateAtMB int) (*Ledger, error) {
	if rotateAtMB <= 0 {
		rotateAtMB = DefaultRotateAtMB
	}
	dir := filepath.Dir(basePath)
	base := filepath.Base(basePath)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	if ext == "" {
		ext = ".jsonl"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("ledger: creating dir %s: %w", dir, err)
	}
	return &Ledger{
		dir:           dir,
		name:          name,
		ext:           ext,
		lockPath:      filepath.Join(dir, "."+name+".lock"),
		metaPath:      filepath.Join(dir, "."+name+".meta.json"),
		rotateAtBytes: int64(rotateAtMB) * 1024 * 1024,
	}, nil
}

func (l *Ledger) segmentPath(index int) string {
	if index == 0 {
		return filepath.Join(l.dir, l.name+l.ext)
	}
	return filepath.Join(l.dir, fmt.Sprintf("%s.%03d%s", l.name, index, l.ext))
}

func (l *Ledger) readMeta() (meta, error) {
	b, err := os.ReadFile(l.metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return meta{CurrentIndex: 0}, nil
		}
		return meta{}, err
	}
	var m meta
	if err := json.Unmarshal(b, &m); err != nil {
		return meta{}, err
	}
	return m, nil
}

func (l *Ledger) writeMeta(m meta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := l.metaPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.metaPath)
}

// lastEntry reads the last line of segPath and parses it as an Entry.
// Returns (Entry{}, false, nil) if the segment doesn't exist or is empty.
// This seeks from the end of the file rather than scanning from the
// start, so cost is independent of segment size.
func lastEntry(segPath string) (Entry, bool, error) {
	f, err := os.Open(segPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Entry{}, false, nil
		}
		return Entry{}, false, err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return Entry{}, false, err
	}
	size := stat.Size()
	if size == 0 {
		return Entry{}, false, nil
	}

	const chunk = 4096
	var buf []byte
	pos := size
	for {
		readSize := int64(chunk)
		if readSize > pos {
			readSize = pos
		}
		pos -= readSize
		part := make([]byte, readSize)
		if _, err := f.ReadAt(part, pos); err != nil && err != io.EOF {
			return Entry{}, false, err
		}
		buf = append(part, buf...)
		// Trim exactly one trailing newline (file ends with "...}\n").
		trimmed := bytes.TrimRight(buf, "\n")
		if idx := bytes.LastIndexByte(trimmed, '\n'); idx >= 0 || pos == 0 {
			line := trimmed
			if idx >= 0 {
				line = trimmed[idx+1:]
			}
			if len(line) == 0 {
				return Entry{}, false, nil
			}
			var e Entry
			if err := json.Unmarshal(line, &e); err != nil {
				return Entry{}, false, fmt.Errorf("ledger: corrupt last line of %s: %w", segPath, err)
			}
			return e, true, nil
		}
		if pos == 0 {
			break
		}
	}
	return Entry{}, false, nil
}

// AppendResult carries what actually got written by one Append call,
// including any ledger.seal / ledger.rotate housekeeping entries that
// were written ahead of the requested entry because rotation triggered.
type AppendResult struct {
	Entry     Entry
	Rotated   bool
	Housekeep []Entry
}

// Append writes one entry under the global ledger lock, handling segment
// rotation transparently. run/src/type/payload describe the requested
// entry; ts defaults to time.Now().UTC() if zero.
func (l *Ledger) Append(run, src, typ string, payload interface{}, ts time.Time) (AppendResult, error) {
	lock, err := acquireLock(l.lockPath)
	if err != nil {
		return AppendResult{}, fmt.Errorf("ledger: acquiring lock: %w", err)
	}
	defer lock.release()

	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return AppendResult{}, fmt.Errorf("ledger: marshaling payload: %w", err)
	}

	var result AppendResult

	m, err := l.readMeta()
	if err != nil {
		return AppendResult{}, fmt.Errorf("ledger: reading meta: %w", err)
	}

	curPath := l.segmentPath(m.CurrentIndex)
	last, hasLast, err := lastEntry(curPath)
	if err != nil {
		return AppendResult{}, err
	}

	// Rotation check: only rotate on a segment that already has content,
	// never rotate an empty fresh segment.
	if hasLast {
		stat, statErr := os.Stat(curPath)
		if statErr == nil && stat.Size() >= l.rotateAtBytes {
			// 1. Seal the current segment.
			sealPayload := map[string]interface{}{
				"segment":       filepath.Base(curPath),
				"terminal_seq":  last.Seq,
				"terminal_hash": last.Hash,
			}
			sealEntry, err := l.appendRaw(curPath, run, "guard", "ledger.seal", sealPayload, last.Seq, last.Hash, ts)
			if err != nil {
				return AppendResult{}, fmt.Errorf("ledger: writing seal: %w", err)
			}
			result.Rotated = true
			result.Housekeep = append(result.Housekeep, sealEntry)

			// 2. Advance to the next segment.
			m.CurrentIndex++
			if err := l.writeMeta(m); err != nil {
				return AppendResult{}, fmt.Errorf("ledger: advancing segment: %w", err)
			}
			curPath = l.segmentPath(m.CurrentIndex)

			// 3. First entry of the new segment records the rotation and
			// references the previous segment's terminal hash.
			rotatePayload := map[string]interface{}{
				"from_segment":  filepath.Base(sealPayload["segment"].(string)),
				"to_segment":    filepath.Base(curPath),
				"terminal_hash": sealEntry.Hash,
			}
			rotateEntry, err := l.appendRaw(curPath, run, "guard", "ledger.rotate", rotatePayload, sealEntry.Seq, sealEntry.Hash, ts)
			if err != nil {
				return AppendResult{}, fmt.Errorf("ledger: writing rotate marker: %w", err)
			}
			result.Housekeep = append(result.Housekeep, rotateEntry)
			last = rotateEntry
			hasLast = true
		}
	}

	prevSeq := 0
	prevHash := ""
	if hasLast {
		prevSeq = last.Seq
		prevHash = last.Hash
	}

	entry, err := l.appendRawBytes(curPath, run, src, typ, payloadBytes, prevSeq, prevHash, ts)
	if err != nil {
		return AppendResult{}, err
	}
	result.Entry = entry
	return result, nil
}

func (l *Ledger) appendRaw(segPath, run, src, typ string, payload interface{}, prevSeq int, prevHash string, ts time.Time) (Entry, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return Entry{}, err
	}
	return l.appendRawBytes(segPath, run, src, typ, b, prevSeq, prevHash, ts)
}

func (l *Ledger) appendRawBytes(segPath, run, src, typ string, payload json.RawMessage, prevSeq int, prevHash string, ts time.Time) (Entry, error) {
	pre := entryPre{
		V:       SchemaVersion,
		Seq:     prevSeq + 1,
		TS:      ts.Format(time.RFC3339Nano),
		Run:     run,
		Src:     src,
		Type:    typ,
		Payload: payload,
		Prev:    prevHash,
	}
	hash, preBytes, err := computeHash(pre)
	if err != nil {
		return Entry{}, err
	}
	// Splice the hash field onto the already-hashed bytes rather than
	// re-marshaling, so the bytes on disk are byte-identical to what was
	// hashed plus the appended hash field.
	line := make([]byte, 0, len(preBytes)+32)
	line = append(line, preBytes[:len(preBytes)-1]...) // drop trailing '}'
	line = append(line, []byte(`,"hash":"`)...)
	line = append(line, []byte(hash)...)
	line = append(line, []byte(`"}`)...)
	line = append(line, '\n')

	f, err := os.OpenFile(segPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return Entry{}, err
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return Entry{}, err
	}

	return Entry{
		V:       pre.V,
		Seq:     pre.Seq,
		TS:      pre.TS,
		Run:     pre.Run,
		Src:     pre.Src,
		Type:    pre.Type,
		Payload: pre.Payload,
		Prev:    pre.Prev,
		Hash:    hash,
	}, nil
}

// segments returns every existing segment path in chain order: base
// segment first (if present), then numbered segments ascending.
func (l *Ledger) segments() ([]string, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var numbered []int
	hasBase := false
	baseName := l.name + l.ext
	prefix := l.name + "."
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if n == baseName {
			hasBase = true
			continue
		}
		if strings.HasPrefix(n, prefix) && strings.HasSuffix(n, l.ext) {
			mid := strings.TrimSuffix(strings.TrimPrefix(n, prefix), l.ext)
			var idx int
			if _, err := fmt.Sscanf(mid, "%03d", &idx); err == nil {
				numbered = append(numbered, idx)
			}
		}
	}
	sort.Ints(numbered)
	var out []string
	if hasBase {
		out = append(out, l.segmentPath(0))
	}
	for _, idx := range numbered {
		out = append(out, l.segmentPath(idx))
	}
	return out, nil
}

// VerifyResult is the outcome of walking the full chain.
type VerifyResult struct {
	OK           bool
	SegmentCount int
	EntryCount   int
	FailedAt     *FailedEntry
}

type FailedEntry struct {
	Segment string
	Seq     int
	Reason  string
}

// Verify walks every segment in order and checks: each entry's stored
// hash matches its recomputed hash, seq increments by exactly 1 across
// the whole chain, and each entry's prev matches the previous entry's
// hash (including across segment boundaries, via ledger.rotate entries).
func (l *Ledger) Verify() (VerifyResult, error) {
	segs, err := l.segments()
	if err != nil {
		return VerifyResult{}, err
	}
	res := VerifyResult{OK: true, SegmentCount: len(segs)}

	prevHash := ""
	prevSeq := 0
	first := true

	for _, segPath := range segs {
		f, err := os.Open(segPath)
		if err != nil {
			return VerifyResult{}, err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var e Entry
			if err := json.Unmarshal(line, &e); err != nil {
				f.Close()
				res.OK = false
				res.FailedAt = &FailedEntry{Segment: filepath.Base(segPath), Reason: "unparseable entry: " + err.Error()}
				return res, nil
			}
			res.EntryCount++

			pre := entryPre{V: e.V, Seq: e.Seq, TS: e.TS, Run: e.Run, Src: e.Src, Type: e.Type, Payload: e.Payload, Prev: e.Prev}
			wantHash, _, err := computeHash(pre)
			if err != nil {
				f.Close()
				return VerifyResult{}, err
			}
			if wantHash != e.Hash {
				f.Close()
				res.OK = false
				res.FailedAt = &FailedEntry{Segment: filepath.Base(segPath), Seq: e.Seq, Reason: "hash mismatch (entry was tampered with)"}
				return res, nil
			}
			if !first && e.Seq != prevSeq+1 {
				f.Close()
				res.OK = false
				res.FailedAt = &FailedEntry{Segment: filepath.Base(segPath), Seq: e.Seq, Reason: fmt.Sprintf("sequence gap: expected %d, got %d", prevSeq+1, e.Seq)}
				return res, nil
			}
			if !first && e.Prev != prevHash {
				f.Close()
				res.OK = false
				res.FailedAt = &FailedEntry{Segment: filepath.Base(segPath), Seq: e.Seq, Reason: "prev hash does not match preceding entry (chain broken)"}
				return res, nil
			}
			prevHash = e.Hash
			prevSeq = e.Seq
			first = false
		}
		if err := scanner.Err(); err != nil {
			f.Close()
			return VerifyResult{}, err
		}
		f.Close()
	}
	return res, nil
}

// Tail returns up to n most recent entries across all segments, in
// chronological order, for `centrol audit` / `centrol status`.
func (l *Ledger) Tail(n int) ([]Entry, error) {
	segs, err := l.segments()
	if err != nil {
		return nil, err
	}
	var all []Entry
	for _, segPath := range segs {
		f, err := os.Open(segPath)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var e Entry
			if err := json.Unmarshal(line, &e); err != nil {
				f.Close()
				return nil, err
			}
			all = append(all, e)
		}
		f.Close()
		if err := scanner.Err(); err != nil {
			return nil, err
		}
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

// LastHashSeq returns the current chain head (hash, seq) so a caller
// (e.g. the Governor) can resume without re-walking the whole chain.
// Returns ("", 0, false) for a brand-new ledger.
func (l *Ledger) LastHashSeq() (string, int, bool, error) {
	m, err := l.readMeta()
	if err != nil {
		return "", 0, false, err
	}
	e, ok, err := lastEntry(l.segmentPath(m.CurrentIndex))
	if err != nil {
		return "", 0, false, err
	}
	if !ok {
		return "", 0, false, nil
	}
	return e.Hash, e.Seq, true, nil
}
