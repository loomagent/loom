package loomfs

import (
	"os"
	"path/filepath"
)

func materialize(events []JournalEvent, root string) ContextSnapshot {
	queries := map[string]QueryRecord{}
	sources := map[string]SourceEntry{}
	priorTurns := map[uint64]PriorTurn{}
	turnContexts := map[uint64]TurnContext{}

	for _, ev := range events {
		switch ev.Type {
		case eventTypeSearchObserved:
			if ev.Query == nil || ev.Query.QueryID == "" {
				continue
			}
			queries[ev.Query.QueryID] = *ev.Query
		case eventTypeSourceSaved:
			if ev.Source == nil || ev.Source.ID == "" {
				continue
			}
			src := *ev.Source
			if existing, ok := sources[src.ID]; ok {
				src.FoundByQueries = uniqueStrings(append(existing.FoundByQueries, src.FoundByQueries...))
				if src.FoundByQuery == "" {
					src.FoundByQuery = existing.FoundByQuery
				}
				if src.Summary == "" {
					src.Summary = existing.Summary
				}
				if src.Tier == "" {
					src.Tier = existing.Tier
				}
				if src.Date == "" {
					src.Date = existing.Date
				}
				if src.DateSource == "" {
					src.DateSource = existing.DateSource
				}
				if src.PublishedAt == nil {
					src.PublishedAt = existing.PublishedAt
				}
				if src.PublishedDateText == "" {
					src.PublishedDateText = existing.PublishedDateText
				}
				if src.PublishedDateSource == "" {
					src.PublishedDateSource = existing.PublishedDateSource
				}
				if src.PublishedDateConfidence == "" {
					src.PublishedDateConfidence = existing.PublishedDateConfidence
				}
			}
			if src.FoundByQuery == "" && len(src.FoundByQueries) > 0 {
				src.FoundByQuery = src.FoundByQueries[0]
			}
			sources[src.ID] = src
			for qid, rec := range queries {
				changed := false
				for i := range rec.Hits {
					if NormalizeURL(rec.Hits[i].URL) == NormalizeURL(src.URL) {
						rec.Hits[i].SrcID = src.ID
						changed = true
					}
				}
				if changed {
					rec.NumSaved = countSavedHits(rec.Hits)
					queries[qid] = rec
				}
			}
		case eventTypeTurnCompleted:
			if ev.PriorTurn == nil || ev.PriorTurn.TurnIndex == 0 {
				continue
			}
			priorTurns[ev.PriorTurn.TurnIndex] = *ev.PriorTurn
		case eventTypeTurnContext:
			if ev.Context == nil || ev.Context.TurnIndex == 0 {
				continue
			}
			turnContexts[ev.Context.TurnIndex] = *ev.Context
		default:
		}
	}

	rawCount := uint64(0)
	if entries, err := os.ReadDir(filepath.Join(root, rawDirName)); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && filepath.Ext(entry.Name()) == rawExt {
				rawCount++
			}
		}
	}

	return ContextSnapshot{
		PriorTurns:   sortedPriorTurns(priorTurns),
		TurnContexts: sortedTurnContexts(turnContexts),
		Queries:      sortedQueryRecords(queries),
		Sources:      sortedSourceEntries(sources),
		Stats:        SnapshotStats{RawCount: rawCount},
	}
}

func countSavedHits(hits []QueryHit) uint64 {
	var n uint64
	for _, h := range hits {
		if h.SrcID != "" {
			n++
		}
	}
	return n
}
