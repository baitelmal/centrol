package policy

import (
	"bufio"
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// tomlDoc is [section][key]value for the small subset of TOML centrol's
// own config files use: [section] headers, and key = value lines where
// value is a quoted string, a bare integer, a bool, or an array of
// quoted strings. This is intentionally not a general TOML parser —
// pulling a full TOML dependency for a handful of well-known config
// keys would be more surface area than the config format itself,
// consistent with the Governor's own hand-rolled schema interpreter
// rather than a full JSON-Schema engine.
type tomlDoc map[string]map[string]interface{}

func parseTOMLSubset(data []byte) (tomlDoc, error) {
	doc := tomlDoc{}
	section := ""
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			if _, ok := doc[section]; !ok {
				doc[section] = map[string]interface{}{}
			}
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			return nil, fmt.Errorf("line %d: expected key = value, got %q", lineNo, line)
		}
		key := strings.TrimSpace(line[:eq])
		raw := strings.TrimSpace(line[eq+1:])
		val, err := parseTOMLValue(raw)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if section == "" {
			return nil, fmt.Errorf("line %d: key %q outside any [section]", lineNo, key)
		}
		doc[section][key] = val
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return doc, nil
}

func parseTOMLValue(raw string) (interface{}, error) {
	switch {
	case raw == "true":
		return true, nil
	case raw == "false":
		return false, nil
	case strings.HasPrefix(raw, `"`) && strings.HasSuffix(raw, `"`) && len(raw) >= 2:
		return raw[1 : len(raw)-1], nil
	case strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]"):
		inner := strings.TrimSpace(raw[1 : len(raw)-1])
		if inner == "" {
			return []string{}, nil
		}
		var out []string
		for _, part := range strings.Split(inner, ",") {
			part = strings.TrimSpace(part)
			if !strings.HasPrefix(part, `"`) || !strings.HasSuffix(part, `"`) || len(part) < 2 {
				return nil, fmt.Errorf("array element %q is not a quoted string (only string arrays are supported)", part)
			}
			out = append(out, part[1:len(part)-1])
		}
		return out, nil
	default:
		if n, err := strconv.Atoi(raw); err == nil {
			return n, nil
		}
		return nil, fmt.Errorf("unsupported value %q (supported: quoted strings, integers, true/false, string arrays)", raw)
	}
}

// writeTOMLSubset renders doc back to text, sections and keys sorted for
// deterministic output (stable diffs when centrol rewrites a config
// file, e.g. via `centrol config` or "Add to allowlist").
func writeTOMLSubset(doc tomlDoc) []byte {
	var sections []string
	for s := range doc {
		sections = append(sections, s)
	}
	sort.Strings(sections)

	var b bytes.Buffer
	for i, s := range sections {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[%s]\n", s)
		var keys []string
		for k := range doc[s] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s = %s\n", k, formatTOMLValue(doc[s][k]))
		}
	}
	return b.Bytes()
}

func formatTOMLValue(v interface{}) string {
	switch val := v.(type) {
	case bool:
		if val {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(val)
	case string:
		return strconv.Quote(val)
	case []string:
		quoted := make([]string, len(val))
		for i, s := range val {
			quoted[i] = strconv.Quote(s)
		}
		return "[" + strings.Join(quoted, ", ") + "]"
	default:
		return strconv.Quote(fmt.Sprintf("%v", val))
	}
}
