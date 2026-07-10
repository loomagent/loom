package loomfs

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func cleanRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", fmt.Errorf("loomfs: root 目录不能为空")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("loomfs: 解析根目录绝对路径失败: %w", err)
	}
	return abs, nil
}

func forEachJSONLLine(r io.Reader, fn func([]byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if err := fn([]byte(line)); err != nil {
			return err
		}
	}
	return sc.Err()
}

func writeJSONL(path string, values anySlice) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, v := range values.items() {
		if err := enc.Encode(v); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type anySlice interface {
	items() []any
}

type queryRecords []QueryRecord

func (q queryRecords) items() []any {
	out := make([]any, 0, len(q))
	for i := range q {
		out = append(out, q[i])
	}
	return out
}

type sourceEntries []SourceEntry

func (s sourceEntries) items() []any {
	out := make([]any, 0, len(s))
	for i := range s {
		out = append(out, s[i])
	}
	return out
}

type priorTurns []PriorTurn

func (p priorTurns) items() []any {
	out := make([]any, 0, len(p))
	for i := range p {
		out = append(out, p[i])
	}
	return out
}

type turnContexts []TurnContext

func (t turnContexts) items() []any {
	out := make([]any, 0, len(t))
	for i := range t {
		out = append(out, t[i])
	}
	return out
}

func NormalizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return strings.ToLower(raw)
	}
	parsed.Fragment = ""
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	return strings.TrimRight(parsed.String(), "/")
}

func NormalizeQuery(query string) string {
	return strings.ToLower(strings.Join(strings.Fields(query), " "))
}

func domainOf(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Host)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string, limit int) string {
	s = oneLine(s)
	if limit <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "..."
}

func srcID(seq uint64) string {
	return "SRC-" + strconv.FormatUint(seq, 10)
}

func queryID(seq uint64) string {
	return "QUERY-" + strconv.FormatUint(seq, 10)
}

func parseIDSeq(id, prefix string) uint64 {
	if !strings.HasPrefix(id, prefix) {
		return 0
	}
	n, _ := strconv.ParseUint(strings.TrimPrefix(id, prefix), 10, 64)
	return n
}

func rawPath(id string) string {
	return filepath.ToSlash(filepath.Join(rawDirName, id+rawExt))
}

func sortedQueryRecords(records map[string]QueryRecord) []QueryRecord {
	out := make([]QueryRecord, 0, len(records))
	for _, rec := range records {
		out = append(out, rec)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return parseIDSeq(out[i].QueryID, "QUERY-") < parseIDSeq(out[j].QueryID, "QUERY-")
	})
	return out
}

func sortedSourceEntries(entries map[string]SourceEntry) []SourceEntry {
	out := make([]SourceEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return parseIDSeq(out[i].ID, "SRC-") < parseIDSeq(out[j].ID, "SRC-")
	})
	return out
}

func sortedPriorTurns(turns map[uint64]PriorTurn) []PriorTurn {
	out := make([]PriorTurn, 0, len(turns))
	for _, turn := range turns {
		out = append(out, turn)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TurnIndex < out[j].TurnIndex })
	return out
}

func sortedTurnContexts(contexts map[uint64]TurnContext) []TurnContext {
	out := make([]TurnContext, 0, len(contexts))
	for _, ctx := range contexts {
		out = append(out, ctx)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TurnIndex < out[j].TurnIndex })
	return out
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
