package loomfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/loomagent/loom"
)

type TurnSession struct {
	ws   *Workspace
	meta TurnMeta

	mu                     sync.Mutex
	nextQuerySeq           uint64
	nextSourceSeq          uint64
	turnQueryIndex         uint64
	pendingQueries         []QueryRecord
	pendingSources         map[string]SourceEntry
	pendingObservations    map[string]SourceContentObservation
	flushedQueries         map[string]struct{}
	flushedSources         map[string]struct{}
	flushedObservations    map[string]struct{}
	urlToPendingHits       map[string][]pendingHitRef
	sourceByURL            map[string]SourceEntry
	sourceByID             map[string]SourceEntry
	queryIDsByURL          map[string][]string
	turnQueryIDsByURL      map[string][]string
	currentObservedSources map[string]struct{}
	observationByKey       map[string]SourceContentObservation
	searchMetaByURL        map[string]sourceSearchMeta
	seenQueryText          map[string]struct{}
	artifactPaths          []string
	finishedPriorTurn      bool
}

type pendingHitRef struct {
	queryIdx int
	hitIdx   int
}

type sourceSearchMeta struct {
	Title      string
	Snippet    string
	Date       string
	DateSource string
}

func newTurnSession(ws *Workspace, meta TurnMeta, snapshot ContextSnapshot) *TurnSession {
	s := &TurnSession{
		ws:                     ws,
		meta:                   meta,
		nextQuerySeq:           1,
		nextSourceSeq:          1,
		pendingSources:         map[string]SourceEntry{},
		pendingObservations:    map[string]SourceContentObservation{},
		flushedQueries:         map[string]struct{}{},
		flushedSources:         map[string]struct{}{},
		flushedObservations:    map[string]struct{}{},
		urlToPendingHits:       map[string][]pendingHitRef{},
		sourceByURL:            map[string]SourceEntry{},
		sourceByID:             map[string]SourceEntry{},
		queryIDsByURL:          map[string][]string{},
		turnQueryIDsByURL:      map[string][]string{},
		currentObservedSources: map[string]struct{}{},
		observationByKey:       map[string]SourceContentObservation{},
		searchMetaByURL:        map[string]sourceSearchMeta{},
		seenQueryText:          map[string]struct{}{},
	}
	for _, q := range snapshot.Queries {
		if seq := parseIDSeq(q.QueryID, "QUERY-"); seq >= s.nextQuerySeq {
			s.nextQuerySeq = seq + 1
		}
		// Query identities are conversation-global, but duplicate suppression is
		// execution-local. A new turn or retry must be allowed to issue the same
		// useful query.
		if sameExecution(q.TurnIndex, q.ExecutionID, meta) {
			if text := NormalizeQuery(q.Text); text != "" {
				s.seenQueryText[text] = struct{}{}
			}
		}
		for _, h := range q.Hits {
			key := NormalizeURL(h.URL)
			if key != "" {
				s.queryIDsByURL[key] = uniqueStrings(append(s.queryIDsByURL[key], q.QueryID))
				if sameExecution(q.TurnIndex, q.ExecutionID, meta) {
					s.turnQueryIDsByURL[key] = uniqueStrings(append(s.turnQueryIDsByURL[key], q.QueryID))
				}
				s.mergeSearchMetaLocked(key, sourceSearchMeta{
					Title:      h.Title,
					Snippet:    h.Snippet,
					Date:       h.Date,
					DateSource: h.DateSource,
				})
			}
		}
	}
	for _, src := range snapshot.Sources {
		if seq := parseIDSeq(src.ID, "SRC-"); seq >= s.nextSourceSeq {
			s.nextSourceSeq = seq + 1
		}
		if key := NormalizeURL(src.URL); key != "" {
			s.sourceByURL[key] = src
		}
		if strings.TrimSpace(src.ID) != "" {
			s.sourceByID[src.ID] = src
		}
	}
	for _, observation := range snapshot.SourceObservations {
		key := sourceObservationKey(observation.TurnIndex, observation.ExecutionID, observation.SourceID, observation.ContentSHA256)
		if key != "" {
			s.observationByKey[key] = observation
		}
		if sameExecution(observation.TurnIndex, observation.ExecutionID, meta) && strings.TrimSpace(observation.SourceID) != "" {
			s.currentObservedSources[observation.SourceID] = struct{}{}
		}
	}
	return s
}

func (s *TurnSession) Snapshot(ctx context.Context, opts SnapshotOptions) (ContextSnapshot, error) {
	if s == nil || s.ws == nil {
		return ContextSnapshot{}, fmt.Errorf("loomfs: turn session 为空")
	}
	return s.ws.LoadSnapshot(ctx, opts)
}

func (s *TurnSession) Meta() TurnMeta {
	if s == nil {
		return TurnMeta{}
	}
	return s.meta
}

func (s *TurnSession) HasQuery(query string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.seenQueryText[NormalizeQuery(query)]
	return ok
}

func (s *TurnSession) HasObservedSource(sourceID string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.currentObservedSources[strings.TrimSpace(sourceID)]
	return ok
}

func (s *TurnSession) ObserveSearch(_ context.Context, obs SearchObservation) (QueryRecord, error) {
	if s == nil {
		return QueryRecord{}, nil
	}
	query := strings.TrimSpace(obs.Query)
	if query == "" {
		return QueryRecord{}, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.turnQueryIndex++
	rec := QueryRecord{
		QueryID:        queryID(s.nextQuerySeq),
		TurnIndex:      s.meta.TurnIndex,
		ExecutionID:    strings.TrimSpace(s.meta.ExecutionID),
		TurnQueryIndex: s.turnQueryIndex,
		Executor:       strings.TrimSpace(s.meta.Executor),
		Tool:           strings.TrimSpace(obs.Tool),
		Phase:          strings.TrimSpace(obs.Phase),
		Round:          obs.Round,
		Text:           query,
		Why:            strings.TrimSpace(obs.Why),
		Hits:           make([]QueryHit, 0, len(obs.Hits)),
		At:             time.Now(),
	}
	s.nextQuerySeq++
	s.seenQueryText[NormalizeQuery(query)] = struct{}{}

	for i, hit := range obs.Hits {
		qh := QueryHit{
			Pos:        uint64(i + 1),
			URL:        strings.TrimSpace(hit.URL),
			Title:      strings.TrimSpace(hit.Title),
			Snippet:    strings.TrimSpace(hit.Snippet),
			Date:       strings.TrimSpace(hit.Date),
			DateSource: strings.TrimSpace(hit.DateSource),
			Relevant:   hit.Relevant,
			SrcID:      strings.TrimSpace(hit.SrcID),
		}
		if qh.URL != "" && qh.SrcID == "" {
			if src, ok := s.sourceByURL[NormalizeURL(qh.URL)]; ok {
				qh.SrcID = src.ID
			}
		}
		if qh.Relevant {
			rec.NumRelevant++
		}
		rec.Hits = append(rec.Hits, qh)
		key := NormalizeURL(qh.URL)
		if key != "" {
			s.queryIDsByURL[key] = uniqueStrings(append(s.queryIDsByURL[key], rec.QueryID))
			s.turnQueryIDsByURL[key] = uniqueStrings(append(s.turnQueryIDsByURL[key], rec.QueryID))
			s.mergeSearchMetaLocked(key, sourceSearchMeta{
				Title:      qh.Title,
				Snippet:    qh.Snippet,
				Date:       qh.Date,
				DateSource: qh.DateSource,
			})
			s.urlToPendingHits[key] = append(s.urlToPendingHits[key], pendingHitRef{
				queryIdx: len(s.pendingQueries),
				hitIdx:   len(rec.Hits) - 1,
			})
		}
	}
	rec.NumResults = uint64(len(rec.Hits))
	rec.NumSaved = countSavedHits(rec.Hits)
	// The session owns the persisted copy. Callers may retain the returned
	// record while source saves update query/source links.
	s.pendingQueries = append(s.pendingQueries, cloneQueryRecord(rec))
	return rec, nil
}

func (s *TurnSession) SaveSource(ctx context.Context, obs SourceObservation) (SourceEntry, bool, error) {
	result, err := s.SaveSourceVersioned(ctx, obs)
	return result.Entry, result.SourceCreated, err
}

func (s *TurnSession) SaveSourceVersioned(_ context.Context, obs SourceObservation) (SourceSaveResult, error) {
	if s == nil {
		return SourceSaveResult{}, nil
	}
	rawURL := strings.TrimSpace(obs.URL)
	markdown := strings.TrimSpace(obs.Markdown)
	if rawURL == "" || markdown == "" {
		return SourceSaveResult{}, nil
	}
	key := NormalizeURL(rawURL)

	s.mu.Lock()
	defer s.mu.Unlock()

	added := false
	entry, exists := s.sourceByURL[key]
	if !exists {
		// 编号优先用调用方显式传入的(sourceregistry 统一分配);
		// 留空才退回 loomfs 自分配(仅测试/无 DB 场景)。
		id := strings.TrimSpace(obs.SrcID)
		if id == "" {
			id = srcID(s.nextSourceSeq)
		}
		if err := validateSourceID(id); err != nil {
			return SourceSaveResult{}, err
		}
		if prior, ok := s.sourceByID[id]; ok && NormalizeURL(prior.URL) != key {
			return SourceSaveResult{}, fmt.Errorf("loomfs: source id %s already belongs to %s", id, prior.URL)
		}
		if seq := parseIDSeq(id, "SRC-"); seq >= s.nextSourceSeq {
			s.nextSourceSeq = seq + 1
		}
		entry = SourceEntry{
			ID:               id,
			URL:              rawURL,
			Domain:           domainOf(rawURL),
			RawPath:          rawPath(id),
			FoundTurnIndex:   s.meta.TurnIndex,
			FoundExecutionID: strings.TrimSpace(s.meta.ExecutionID),
			FoundExecutor:    strings.TrimSpace(s.meta.Executor),
			FoundTool:        strings.TrimSpace(obs.Tool),
			FoundPhase:       strings.TrimSpace(obs.Phase),
			FoundRound:       obs.Round,
			SavedAt:          time.Now(),
		}
		added = true
	}
	entry.Title = firstNonEmpty(entry.Title, obs.Title)
	entry.Snippet = firstNonEmpty(entry.Snippet, obs.Snippet)
	entry.Date = firstNonEmpty(entry.Date, obs.Date)
	entry.DateSource = firstNonEmpty(entry.DateSource, obs.DateSource)
	if entry.PublishedAt == nil && obs.PublishedAt != nil {
		publishedAt := *obs.PublishedAt
		entry.PublishedAt = &publishedAt
	}
	entry.PublishedDateText = firstNonEmpty(entry.PublishedDateText, obs.PublishedDateText)
	entry.PublishedDateSource = firstNonEmpty(entry.PublishedDateSource, obs.PublishedDateSource)
	entry.PublishedDateConfidence = firstNonEmpty(entry.PublishedDateConfidence, obs.PublishedDateConfidence)
	if meta, ok := s.searchMetaByURL[key]; ok {
		entry.Title = firstNonEmpty(entry.Title, meta.Title)
		entry.Snippet = firstNonEmpty(entry.Snippet, meta.Snippet)
		entry.Date = firstNonEmpty(entry.Date, meta.Date)
		entry.DateSource = firstNonEmpty(entry.DateSource, meta.DateSource)
	}
	entry.Summary = firstNonEmpty(entry.Summary, obs.Summary)
	entry.Tier = firstNonEmpty(entry.Tier, obs.Tier)
	if added {
		entry.Chars = uint64(len([]rune(markdown)))
	}
	if entry.RawPath == "" {
		entry.RawPath = rawPath(entry.ID)
	}

	queryIDs := append([]string{}, entry.FoundByQueries...)
	queryIDs = append(queryIDs, s.queryIDsByURL[key]...)
	entry.FoundByQueries = uniqueStrings(queryIDs)
	if len(entry.FoundByQueries) > 0 {
		entry.FoundByQuery = entry.FoundByQueries[0]
	}

	if refs := s.urlToPendingHits[key]; len(refs) > 0 {
		for _, ref := range refs {
			if ref.queryIdx < 0 || ref.queryIdx >= len(s.pendingQueries) {
				continue
			}
			rec := &s.pendingQueries[ref.queryIdx]
			if ref.hitIdx < 0 || ref.hitIdx >= len(rec.Hits) {
				continue
			}
			if rec.Hits[ref.hitIdx].SrcID == "" {
				rec.Hits[ref.hitIdx].SrcID = entry.ID
			}
			rec.NumSaved = countSavedHits(rec.Hits)
		}
	}

	if added {
		if err := os.MkdirAll(filepath.Join(s.ws.root, rawDirName), 0o755); err != nil {
			return SourceSaveResult{}, fmt.Errorf("loomfs: create raw dir: %w", err)
		}
		if err := os.WriteFile(filepath.Join(s.ws.root, entry.RawPath), []byte(markdown), 0o644); err != nil {
			return SourceSaveResult{}, fmt.Errorf("loomfs: write raw source: %w", err)
		}
	}
	digest := sha256.Sum256([]byte(markdown))
	contentSHA := hex.EncodeToString(digest[:])
	contentPath := sourceContentPath(contentSHA)
	if err := writeImmutableContentObject(s.ws.root, contentPath, markdown); err != nil {
		return SourceSaveResult{}, err
	}
	observationKey := sourceObservationKey(s.meta.TurnIndex, s.meta.ExecutionID, entry.ID, contentSHA)
	observation, observed := s.observationByKey[observationKey]
	if !observed {
		now := time.Now()
		observation = SourceContentObservation{
			ObservationID:           sourceObservationID(s.meta.TurnIndex, s.meta.ExecutionID, entry.ID, contentSHA),
			SourceID:                entry.ID,
			TurnIndex:               s.meta.TurnIndex,
			ExecutionID:             strings.TrimSpace(s.meta.ExecutionID),
			QueryIDs:                append([]string(nil), s.turnQueryIDsByURL[key]...),
			URL:                     rawURL,
			Title:                   strings.TrimSpace(obs.Title),
			Snippet:                 strings.TrimSpace(obs.Snippet),
			Date:                    strings.TrimSpace(obs.Date),
			DateSource:              strings.TrimSpace(obs.DateSource),
			PublishedAt:             cloneTime(obs.PublishedAt),
			PublishedDateText:       strings.TrimSpace(obs.PublishedDateText),
			PublishedDateSource:     strings.TrimSpace(obs.PublishedDateSource),
			PublishedDateConfidence: strings.TrimSpace(obs.PublishedDateConfidence),
			Domain:                  domainOf(rawURL),
			Summary:                 strings.TrimSpace(obs.Summary),
			Tier:                    strings.TrimSpace(obs.Tier),
			ContentPath:             contentPath,
			ContentSHA256:           contentSHA,
			Chars:                   uint64(len([]rune(markdown))),
			Tool:                    strings.TrimSpace(obs.Tool),
			Phase:                   strings.TrimSpace(obs.Phase),
			Round:                   obs.Round,
			ObservedAt:              now,
		}
		s.observationByKey[observationKey] = observation
		s.pendingObservations[observation.ObservationID] = observation
	}
	s.sourceByURL[key] = entry
	s.sourceByID[entry.ID] = entry
	s.pendingSources[entry.ID] = entry
	s.currentObservedSources[entry.ID] = struct{}{}
	return SourceSaveResult{
		Entry: entry, Observation: observation, SourceCreated: added, ObservationCreated: !observed,
	}, nil
}

func sourceContentPath(contentSHA string) string {
	return filepath.ToSlash(filepath.Join("objects", "sha256", contentSHA[:2], contentSHA+rawExt))
}

func sourceObservationKey(turnIndex uint64, executionID, sourceID, contentSHA string) string {
	executionID = strings.TrimSpace(executionID)
	sourceID = strings.TrimSpace(sourceID)
	contentSHA = strings.TrimSpace(contentSHA)
	if turnIndex == 0 || sourceID == "" || contentSHA == "" {
		return ""
	}
	return fmt.Sprintf("%d:%s:%s:%s", turnIndex, executionID, sourceID, contentSHA)
}

func sourceObservationID(turnIndex uint64, executionID, sourceID, contentSHA string) string {
	executionID = strings.TrimSpace(executionID)
	if executionID == "" {
		return fmt.Sprintf("OBS-%d-%s-%s", turnIndex, strings.TrimPrefix(sourceID, "SRC-"), contentSHA[:16])
	}
	digest := sha256.Sum256([]byte(executionID))
	return fmt.Sprintf("OBS-%d-%s-%s-%s", turnIndex, strings.TrimPrefix(sourceID, "SRC-"), hex.EncodeToString(digest[:])[:8], contentSHA[:16])
}

func sameExecution(turnIndex uint64, executionID string, meta TurnMeta) bool {
	if turnIndex != meta.TurnIndex {
		return false
	}
	current := strings.TrimSpace(meta.ExecutionID)
	if current == "" {
		return strings.TrimSpace(executionID) == ""
	}
	return strings.TrimSpace(executionID) == current
}

func writeImmutableContentObject(root, relativePath, content string) error {
	fullPath := filepath.Join(root, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("loomfs: create source object dir: %w", err)
	}
	f, err := os.OpenFile(fullPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o444)
	if err != nil {
		if !os.IsExist(err) {
			return fmt.Errorf("loomfs: create source object: %w", err)
		}
		existing, readErr := os.ReadFile(fullPath)
		if readErr != nil {
			return fmt.Errorf("loomfs: verify source object: %w", readErr)
		}
		if string(existing) != content {
			return fmt.Errorf("loomfs: source object hash collision at %s", relativePath)
		}
		return nil
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(fullPath)
		return fmt.Errorf("loomfs: write source object: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(fullPath)
		return fmt.Errorf("loomfs: close source object: %w", err)
	}
	return nil
}

func (s *TurnSession) mergeSearchMetaLocked(key string, meta sourceSearchMeta) {
	if key == "" {
		return
	}
	existing := s.searchMetaByURL[key]
	existing.Title = firstNonEmpty(existing.Title, strings.TrimSpace(meta.Title))
	existing.Snippet = firstNonEmpty(existing.Snippet, strings.TrimSpace(meta.Snippet))
	existing.Date = firstNonEmpty(existing.Date, strings.TrimSpace(meta.Date))
	existing.DateSource = firstNonEmpty(existing.DateSource, strings.TrimSpace(meta.DateSource))
	s.searchMetaByURL[key] = existing
}

func (s *TurnSession) Checkpoint(_ context.Context) error {
	if s == nil {
		return nil
	}
	events := s.collectUnflushedEvents(false, nil)
	return s.ws.appendEvents(events)
}

func (s *TurnSession) RecordArtifact(path string) error {
	if s == nil {
		return nil
	}
	cleaned, err := validateArtifactPath(path)
	if err != nil {
		return err
	}
	if s.ws == nil {
		return fmt.Errorf("loomfs: turn session workspace 为空")
	}
	info, err := os.Stat(filepath.Join(s.ws.root, cleaned))
	if err != nil {
		return fmt.Errorf("loomfs: record artifact %s: %w", cleaned, err)
	}
	if info.IsDir() {
		return fmt.Errorf("loomfs: record artifact %s: path is directory", cleaned)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.artifactPaths = uniqueStrings(append(s.artifactPaths, cleaned))
	return nil
}

func (s *TurnSession) Finish(ctx context.Context, out TurnOutcome) error {
	if s == nil {
		return nil
	}
	var completionEvents []JournalEvent
	if out.Turn != nil && out.Turn.Status == loom.TurnStatusCompleted {
		if final := finalAnswerText(out.Turn.Items); strings.TrimSpace(final) != "" {
			finalPath := turnFinalAnswerPath(s.meta.TurnIndex)
			if err := s.writeArtifact(finalPath, final+"\n"); err != nil {
				return err
			}
			now := time.Now()
			pt := PriorTurn{
				TurnIndex:       s.meta.TurnIndex,
				ExecutionID:     strings.TrimSpace(s.meta.ExecutionID),
				ConversationID:  s.meta.ConversationID,
				ChatModeID:      s.meta.ChatModeID,
				Executor:        s.meta.Executor,
				UserText:        s.meta.UserText,
				FinalAnswer:     final,
				FinalAnswerPath: finalPath,
				Status:          string(out.Turn.Status),
				Usage:           out.Turn.Usage,
				At:              now,
			}
			if out.Turn.CloseReason != nil {
				pt.CloseCode = string(out.Turn.CloseReason.Code)
			}
			tc := s.buildTurnContext(final, finalPath, now)
			completionEvents = append(completionEvents,
				JournalEvent{Type: eventTypeTurnCompleted, PriorTurn: &pt, At: now},
				JournalEvent{Type: eventTypeTurnContext, Context: &tc, At: now},
			)
		}
	}
	events := s.collectUnflushedEvents(true, completionEvents)
	if err := s.ws.appendEvents(events); err != nil {
		return err
	}
	_ = ctx
	return nil
}

func (s *TurnSession) buildTurnContext(final string, finalPath string, at time.Time) TurnContext {
	s.mu.Lock()
	defer s.mu.Unlock()

	queryIDs := make([]string, 0, len(s.pendingQueries))
	for _, q := range s.pendingQueries {
		if q.QueryID != "" {
			queryIDs = append(queryIDs, q.QueryID)
		}
	}
	sources := sortedSourceEntries(s.pendingSources)
	sourceIDs := make([]string, 0, len(sources))
	artifactPaths := []string{finalPath}
	artifactPaths = append(artifactPaths, s.artifactPaths...)
	for _, src := range sources {
		if src.ID != "" {
			sourceIDs = append(sourceIDs, src.ID)
		}
		if strings.TrimSpace(src.RawPath) != "" {
			artifactPaths = append(artifactPaths, src.RawPath)
		}
	}
	return TurnContext{
		TurnIndex:      s.meta.TurnIndex,
		ExecutionID:    strings.TrimSpace(s.meta.ExecutionID),
		ConversationID: s.meta.ConversationID,
		ChatModeID:     s.meta.ChatModeID,
		Executor:       s.meta.Executor,
		UserText:       s.meta.UserText,
		FinalSummary:   summaryText(final, 1200),
		QueryIDs:       uniqueStrings(queryIDs),
		SourceIDs:      uniqueStrings(sourceIDs),
		ArtifactPaths:  uniqueStrings(artifactPaths),
		CreatedAt:      at,
	}
}

func (s *TurnSession) writeArtifact(path string, content string) error {
	if s == nil || s.ws == nil {
		return nil
	}
	cleaned, err := validateArtifactPath(path)
	if err != nil {
		return err
	}
	fullPath := filepath.Join(s.ws.root, cleaned)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("loomfs: create artifact dir: %w", err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("loomfs: write artifact %s: %w", cleaned, err)
	}
	return nil
}

func validateArtifactPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" || filepath.IsAbs(path) {
		return "", fmt.Errorf("loomfs: artifact path invalid: %q", path)
	}
	cleaned := filepath.Clean(path)
	if cleaned == "." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) || cleaned == ".." {
		return "", fmt.Errorf("loomfs: artifact path escapes workspace: %q", path)
	}
	return cleaned, nil
}

func turnFinalAnswerPath(turnIndex uint64) string {
	return fmt.Sprintf("%s/%d/final.md", turnsDirName, turnIndex)
}

func (s *TurnSession) collectUnflushedEvents(includeCompletion bool, completionEvents []JournalEvent) []JournalEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := make([]JournalEvent, 0, len(s.pendingQueries)+len(s.pendingSources)+len(s.pendingObservations)+len(completionEvents))
	for i := range s.pendingQueries {
		rec := s.pendingQueries[i]
		if _, ok := s.flushedQueries[rec.QueryID]; ok {
			continue
		}
		r := cloneQueryRecord(rec)
		events = append(events, JournalEvent{Type: eventTypeSearchObserved, Query: &r, At: time.Now()})
		s.flushedQueries[rec.QueryID] = struct{}{}
	}
	for _, src := range s.pendingSources {
		if _, ok := s.flushedSources[src.ID]; ok {
			continue
		}
		entry := cloneSourceEntry(src)
		events = append(events, JournalEvent{Type: eventTypeSourceSaved, Source: &entry, At: time.Now()})
		s.flushedSources[src.ID] = struct{}{}
	}
	for _, observation := range sortedSourceObservations(s.pendingObservations) {
		if _, ok := s.flushedObservations[observation.ObservationID]; ok {
			continue
		}
		entry := cloneSourceObservation(observation)
		events = append(events, JournalEvent{Type: eventTypeSourceObserved, Observation: &entry, At: time.Now()})
		s.flushedObservations[observation.ObservationID] = struct{}{}
	}
	if includeCompletion && len(completionEvents) > 0 && !s.finishedPriorTurn {
		events = append(events, completionEvents...)
		s.finishedPriorTurn = true
	}
	return events
}

func cloneQueryRecord(rec QueryRecord) QueryRecord {
	rec.Hits = append([]QueryHit(nil), rec.Hits...)
	return rec
}

func cloneSourceEntry(entry SourceEntry) SourceEntry {
	entry.FoundByQueries = append([]string(nil), entry.FoundByQueries...)
	if entry.PublishedAt != nil {
		publishedAt := *entry.PublishedAt
		entry.PublishedAt = &publishedAt
	}
	return entry
}

func cloneSourceObservation(observation SourceContentObservation) SourceContentObservation {
	observation.QueryIDs = append([]string(nil), observation.QueryIDs...)
	if observation.PublishedAt != nil {
		publishedAt := *observation.PublishedAt
		observation.PublishedAt = &publishedAt
	}
	return observation
}

func finalAnswerText(items []loom.Item) string {
	for _, item := range items {
		if item.Kind == loom.ItemKindFinalAnswer {
			return strings.TrimSpace(item.Text)
		}
		if len(item.Children) > 0 {
			if text := finalAnswerText(item.Children); text != "" {
				return text
			}
		}
	}
	return ""
}

func summaryText(s string, limit uint64) string {
	s = strings.TrimSpace(s)
	if limit == 0 {
		return s
	}
	runes := []rune(s)
	if uint64(len(runes)) <= limit {
		return s
	}
	return string(runes[:limit]) + "..."
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
