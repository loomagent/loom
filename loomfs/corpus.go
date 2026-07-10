package loomfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Corpus is the read/write compatibility facade for sources.jsonl + raw/.
// New executor code should prefer TurnSession.SaveSource so query/source links
// are captured through the journal.
type Corpus struct {
	ws *Workspace
}

func NewCorpus(root string) (*Corpus, error) {
	ws, err := OpenWorkspace(root)
	if err != nil {
		return nil, err
	}
	return &Corpus{ws: ws}, nil
}

func (c *Corpus) Root() string { return c.ws.root }

func (c *Corpus) SourcesPath() string { return filepath.Join(c.ws.root, sourcesFilename) }

func (c *Corpus) SrcIDByURL(rawURL string) (string, bool) {
	if c == nil || c.ws == nil {
		return "", false
	}
	snapshot, err := c.ws.LoadSnapshot(nilContext(), SnapshotOptions{})
	if err != nil {
		return "", false
	}
	key := NormalizeURL(rawURL)
	for _, src := range snapshot.Sources {
		if NormalizeURL(src.URL) == key {
			return src.ID, true
		}
	}
	return "", false
}

func (c *Corpus) SaveSource(m SourceMeta, rawText string) (SourceEntry, bool, error) {
	if c == nil || c.ws == nil {
		return SourceEntry{}, false, fmt.Errorf("corpus: nil workspace")
	}
	rawURL := strings.TrimSpace(m.URL)
	rawText = strings.TrimSpace(rawText)
	if rawURL == "" {
		return SourceEntry{}, false, fmt.Errorf("corpus: source url is required")
	}
	if rawText == "" {
		return SourceEntry{}, false, fmt.Errorf("corpus: raw text is required")
	}
	snapshot, err := c.ws.LoadSnapshot(nilContext(), SnapshotOptions{})
	if err != nil {
		return SourceEntry{}, false, err
	}
	key := NormalizeURL(rawURL)
	for _, src := range snapshot.Sources {
		if NormalizeURL(src.URL) == key {
			return src, false, nil
		}
	}
	// 编号优先用调用方显式传入的(sourceregistry 统一分配);留空才自分配。
	id := strings.TrimSpace(m.SrcID)
	if id == "" {
		next := uint64(1)
		for _, src := range snapshot.Sources {
			if seq := parseIDSeq(src.ID, "SRC-"); seq >= next {
				next = seq + 1
			}
		}
		id = srcID(next)
	}
	entry := SourceEntry{
		ID:                      id,
		URL:                     rawURL,
		Title:                   strings.TrimSpace(m.Title),
		Snippet:                 strings.TrimSpace(m.Snippet),
		Date:                    strings.TrimSpace(m.Date),
		DateSource:              strings.TrimSpace(m.DateSource),
		PublishedAt:             cloneTime(m.PublishedAt),
		PublishedDateText:       strings.TrimSpace(m.PublishedDateText),
		PublishedDateSource:     strings.TrimSpace(m.PublishedDateSource),
		PublishedDateConfidence: strings.TrimSpace(m.PublishedDateConfidence),
		Domain:                  domainOf(rawURL),
		Summary:                 strings.TrimSpace(m.Summary),
		Tier:                    strings.TrimSpace(m.Tier),
		Chars:                   uint64(len([]rune(rawText))),
		RawPath:                 rawPath(id),
		FoundByQuery:            strings.TrimSpace(m.FoundByQuery),
		FoundByQueries:          uniqueStrings(append(m.FoundByQueries, m.FoundByQuery)),
		FoundTurnIndex:          m.FoundTurnIndex,
		FoundExecutor:           strings.TrimSpace(m.FoundExecutor),
		FoundTool:               strings.TrimSpace(m.FoundTool),
		FoundPhase:              strings.TrimSpace(m.FoundPhase),
		FoundRound:              m.FoundRound,
		SavedAt:                 now(),
	}
	if len(entry.FoundByQueries) > 0 && entry.FoundByQuery == "" {
		entry.FoundByQuery = entry.FoundByQueries[0]
	}
	if err := os.WriteFile(filepath.Join(c.ws.root, entry.RawPath), []byte(rawText), 0o644); err != nil {
		return SourceEntry{}, false, fmt.Errorf("corpus: write raw file: %w", err)
	}
	if err := c.ws.appendEvents([]JournalEvent{{Type: eventTypeSourceSaved, Source: &entry, At: now()}}); err != nil {
		return SourceEntry{}, false, err
	}
	return entry, true, nil
}

func (c *Corpus) Lookup(id string) (SourceEntry, bool) {
	sources, err := c.LoadSources()
	if err != nil {
		return SourceEntry{}, false
	}
	id = strings.TrimSpace(id)
	for _, src := range sources {
		if src.ID == id {
			return src, true
		}
	}
	return SourceEntry{}, false
}

func (c *Corpus) ReadRaw(id string) (string, error) {
	data, err := os.ReadFile(filepath.Join(c.ws.root, rawPath(strings.TrimSpace(id))))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (c *Corpus) LoadSources() ([]SourceEntry, error) {
	snapshot, err := c.ws.LoadSnapshot(nilContext(), SnapshotOptions{})
	if err != nil {
		return nil, err
	}
	return snapshot.Sources, nil
}

func cloneTime(v *time.Time) *time.Time {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}
