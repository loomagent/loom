package loomfs

import (
	"time"

	"github.com/loomagent/loom"
)

const (
	journalFilename    = "journal.jsonl"
	queriesFilename    = "queries.jsonl"
	sourcesFilename    = "sources.jsonl"
	priorTurnsFilename = "prior_turns.jsonl"
	turnContextsFile   = "turn_contexts.jsonl"
	contextDirName     = "context"
	rawDirName         = "raw"
	turnsDirName       = "turns"
	rawExt             = ".md"

	eventTypeSearchObserved = "search_observed"
	eventTypeSourceSaved    = "source_saved"
	eventTypeTurnCompleted  = "turn_completed"
	eventTypeTurnContext    = "turn_context"
)

// TurnMeta describes the current loom turn in conversation-local terms.
//
// TurnIndex is 1-based in loomfs files. The database currently stores a
// 0-based turn index; callers should convert before BeginTurn.
type TurnMeta struct {
	ConversationID string
	TurnIndex      uint64
	ChatModeID     string
	Executor       string
	UserText       string
}

type SearchObservation struct {
	Tool  string
	Phase string
	Round uint64
	Query string
	Why   string
	Hits  []SearchHit
}

type SearchHit struct {
	URL        string
	Title      string
	Snippet    string
	Date       string
	DateSource string
	Relevant   bool
}

type SourceObservation struct {
	Tool                    string
	Phase                   string
	Round                   uint64
	URL                     string
	Title                   string
	Snippet                 string
	Date                    string
	DateSource              string
	PublishedAt             *time.Time
	PublishedDateText       string
	PublishedDateSource     string
	PublishedDateConfidence string
	Markdown                string
	Summary                 string
	Tier                    string
	// SrcID 显式编号(如 "SRC-12")。编号统一由 sourceregistry 分配后传入,
	// 留空时退回 loomfs 自分配(仅测试/无 DB 场景)。
	SrcID string
}

type QueryHit struct {
	Pos        uint64 `json:"pos"`
	URL        string `json:"url"`
	Title      string `json:"title,omitempty"`
	Snippet    string `json:"snippet,omitempty"`
	Date       string `json:"date,omitempty"`
	DateSource string `json:"date_source,omitempty"`
	Relevant   bool   `json:"relevant"`
	SrcID      string `json:"src_id,omitempty"`
}

type QueryRecord struct {
	QueryID        string     `json:"query_id"`
	TurnIndex      uint64     `json:"turn_index,omitempty"`
	TurnQueryIndex uint64     `json:"turn_query_index,omitempty"`
	Executor       string     `json:"executor,omitempty"`
	Tool           string     `json:"tool,omitempty"`
	Phase          string     `json:"phase,omitempty"`
	Round          uint64     `json:"round,omitempty"`
	Text           string     `json:"text"`
	Why            string     `json:"why,omitempty"`
	Hits           []QueryHit `json:"hits,omitempty"`
	NumResults     uint64     `json:"num_results"`
	NumRelevant    uint64     `json:"num_relevant"`
	NumSaved       uint64     `json:"num_saved"`
	At             time.Time  `json:"at"`
}

type SourceEntry struct {
	ID                      string     `json:"src_id"`
	URL                     string     `json:"url"`
	Title                   string     `json:"title,omitempty"`
	Snippet                 string     `json:"snippet,omitempty"`
	Date                    string     `json:"date,omitempty"`
	DateSource              string     `json:"date_source,omitempty"`
	PublishedAt             *time.Time `json:"published_at,omitempty"`
	PublishedDateText       string     `json:"published_date_text,omitempty"`
	PublishedDateSource     string     `json:"published_date_source,omitempty"`
	PublishedDateConfidence string     `json:"published_date_confidence,omitempty"`
	Domain                  string     `json:"domain,omitempty"`
	Summary                 string     `json:"summary,omitempty"`
	Tier                    string     `json:"tier,omitempty"`
	Chars                   uint64     `json:"chars,omitempty"`
	RawPath                 string     `json:"raw_path,omitempty"`
	FoundByQuery            string     `json:"found_by_query,omitempty"`
	FoundByQueries          []string   `json:"found_by_queries,omitempty"`
	FoundTurnIndex          uint64     `json:"found_turn_index,omitempty"`
	FoundExecutor           string     `json:"found_executor,omitempty"`
	FoundTool               string     `json:"found_tool,omitempty"`
	FoundPhase              string     `json:"found_phase,omitempty"`
	FoundRound              uint64     `json:"found_round,omitempty"`
	SavedAt                 time.Time  `json:"saved_at"`
}

type SourceMeta struct {
	URL                     string
	Title                   string
	Snippet                 string
	Date                    string
	DateSource              string
	PublishedAt             *time.Time
	PublishedDateText       string
	PublishedDateSource     string
	PublishedDateConfidence string
	Summary                 string
	Tier                    string
	// SrcID 显式编号(如 "SRC-12"),同 SourceObservation.SrcID。
	SrcID          string
	FoundByQuery   string
	FoundByQueries []string
	FoundTurnIndex uint64
	FoundExecutor  string
	FoundTool      string
	FoundPhase     string
	FoundRound     uint64
}

type PriorTurn struct {
	TurnIndex       uint64     `json:"turn_index"`
	ConversationID  string     `json:"conversation_id,omitempty"`
	ChatModeID      string     `json:"chat_mode_id,omitempty"`
	Executor        string     `json:"executor,omitempty"`
	UserText        string     `json:"user_text,omitempty"`
	FinalAnswer     string     `json:"final_answer,omitempty"`
	FinalAnswerPath string     `json:"final_answer_path,omitempty"`
	Status          string     `json:"status,omitempty"`
	CloseCode       string     `json:"close_code,omitempty"`
	Usage           loom.Usage `json:"usage"`
	At              time.Time  `json:"at"`
}

type TurnContext struct {
	TurnIndex      uint64    `json:"turn_index"`
	ConversationID string    `json:"conversation_id,omitempty"`
	ChatModeID     string    `json:"chat_mode_id,omitempty"`
	Executor       string    `json:"executor,omitempty"`
	UserText       string    `json:"user_text,omitempty"`
	FinalSummary   string    `json:"final_summary,omitempty"`
	QueryIDs       []string  `json:"query_ids,omitempty"`
	SourceIDs      []string  `json:"source_ids,omitempty"`
	ArtifactPaths  []string  `json:"artifact_paths,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type JournalEvent struct {
	Type      string       `json:"type"`
	At        time.Time    `json:"at"`
	Query     *QueryRecord `json:"query,omitempty"`
	Source    *SourceEntry `json:"source,omitempty"`
	PriorTurn *PriorTurn   `json:"prior_turn,omitempty"`
	Context   *TurnContext `json:"context,omitempty"`
}

type ContextSnapshot struct {
	PriorTurns   []PriorTurn
	TurnContexts []TurnContext
	Queries      []QueryRecord
	Sources      []SourceEntry
	Stats        SnapshotStats
}

type SnapshotStats struct {
	RawCount uint64
}

type SnapshotOptions struct{}

type RenderOptions struct {
	MaxPriorTurns uint64
	MaxQueries    uint64
	MaxSources    uint64
}

type TurnOutcome struct {
	Turn *loom.Turn
	Err  error
}
