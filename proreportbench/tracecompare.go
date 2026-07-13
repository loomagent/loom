package proreportbench

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/loomagent/loom"
)

type Signal string

const (
	SignalExpertResearch   Signal = "expert_research"
	SignalBrainstorm       Signal = "brainstorm"
	SignalDataSonar        Signal = "data_sonar"
	SignalDeepRead         Signal = "deep_read"
	SignalCrossVerify      Signal = "cross_verify"
	SignalStartReport      Signal = "start_report"
	SignalUnverifiedNotice Signal = "unverified_notice"
	SignalVerifyCTA        Signal = "verify_cta"
	SignalLongReport       Signal = "long_report"
	SignalForbiddenAudit   Signal = "forbidden_audit"
)

var CoreRequiredSignals = []Signal{
	SignalExpertResearch,
	SignalBrainstorm,
	SignalDataSonar,
	SignalStartReport,
	SignalUnverifiedNotice,
}

type Marker struct {
	Line int    `json:"line,omitempty"`
	Text string `json:"text"`
}

type TraceEvent struct {
	Index    int      `json:"index"`
	Kind     string   `json:"kind"`
	Label    string   `json:"label,omitempty"`
	Text     string   `json:"text,omitempty"`
	Signals  []Signal `json:"signals,omitempty"`
	Path     string   `json:"path,omitempty"`
	Line     int      `json:"line,omitempty"`
	ToolName string   `json:"tool_name,omitempty"`
}

type TraceSummary struct {
	TotalEvents int            `json:"total_events"`
	Counts      map[Signal]int `json:"counts"`
	FirstIndex  map[Signal]int `json:"first_index"`
	Events      []TraceEvent   `json:"events,omitempty"`
	RawCounts   map[string]int `json:"raw_counts,omitempty"`
	Extra       map[string]any `json:"extra,omitempty"`
}

type CompareReport struct {
	Pass         bool         `json:"pass"`
	Reference    TraceSummary `json:"reference"`
	Candidate    TraceSummary `json:"candidate"`
	MissingCore  []Signal     `json:"missing_core"`
	Forbidden    []TraceEvent `json:"forbidden"`
	OrderIssues  []string     `json:"order_issues"`
	CountDeltas  []CountDelta `json:"count_deltas"`
	Observations []string     `json:"observations"`
}

type CountDelta struct {
	Signal    Signal `json:"signal"`
	Reference int    `json:"reference"`
	Candidate int    `json:"candidate"`
	Delta     int    `json:"delta"`
}

func LoadUnifuncsMarkers(r io.Reader) ([]Marker, error) {
	var markers []Marker
	if err := json.NewDecoder(r).Decode(&markers); err != nil {
		return nil, fmt.Errorf("decode unifuncs markers: %w", err)
	}
	return markers, nil
}

func SummarizeUnifuncs(markers []Marker) TraceSummary {
	events := make([]TraceEvent, 0, len(markers))
	for i, m := range markers {
		text := strings.TrimSpace(m.Text)
		events = append(events, TraceEvent{
			Index:   i,
			Kind:    "visible_marker",
			Text:    text,
			Signals: classify(text),
			Line:    m.Line,
		})
	}
	return summarize(events)
}

func SummarizeProReportJSON(r io.Reader) (TraceSummary, error) {
	var raw json.RawMessage
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return TraceSummary{}, fmt.Errorf("decode proreport trace json: %w", err)
	}
	if len(raw) == 0 {
		return TraceSummary{}, fmt.Errorf("empty proreport trace json")
	}

	var markers []Marker
	if err := json.Unmarshal(raw, &markers); err == nil && len(markers) > 0 {
		return SummarizeUnifuncs(markers), nil
	}

	if events := eventsFromGenericJSON(raw); len(events) > 0 {
		return summarize(events), nil
	}

	var turn loom.Turn
	if err := json.Unmarshal(raw, &turn); err == nil && len(turn.Items) > 0 {
		events := eventsFromItems(turn.Items)
		return summarize(events), nil
	}

	var items []loom.Item
	if err := json.Unmarshal(raw, &items); err == nil && len(items) > 0 {
		events := eventsFromItems(items)
		return summarize(events), nil
	}

	return TraceSummary{}, fmt.Errorf("unsupported proreport trace json: expected loom.Turn, []loom.Item, or []Marker")
}

func Compare(reference, candidate TraceSummary) CompareReport {
	report := CompareReport{
		Reference:   reference,
		Candidate:   candidate,
		CountDeltas: make([]CountDelta, 0, len(CoreRequiredSignals)),
	}
	for _, sig := range CoreRequiredSignals {
		if candidate.Counts[sig] == 0 {
			report.MissingCore = append(report.MissingCore, sig)
		}
		report.CountDeltas = append(report.CountDeltas, CountDelta{
			Signal:    sig,
			Reference: reference.Counts[sig],
			Candidate: candidate.Counts[sig],
			Delta:     candidate.Counts[sig] - reference.Counts[sig],
		})
	}
	for _, ev := range candidate.Events {
		if slices.Contains(ev.Signals, SignalForbiddenAudit) {
			report.Forbidden = append(report.Forbidden, ev)
		}
	}
	report.OrderIssues = checkCoreOrder(candidate)
	report.Observations = append(report.Observations, observations(reference, candidate)...)
	report.Pass = len(report.MissingCore) == 0 && len(report.Forbidden) == 0 && len(report.OrderIssues) == 0
	return report
}

func eventsFromItems(items []loom.Item) []TraceEvent {
	events := make([]TraceEvent, 0)
	var walk func([]loom.Item)
	walk = func(items []loom.Item) {
		for _, item := range items {
			text := itemText(item)
			signals := classifyCandidateItem(
				string(item.Kind),
				item.Label,
				item.ToolName,
				item.Arguments,
				item.Output,
				item.Text,
			)
			events = append(events, TraceEvent{
				Index:    len(events),
				Kind:     string(item.Kind),
				Label:    strings.TrimSpace(item.Label),
				Text:     text,
				Signals:  signals,
				Path:     item.Path,
				ToolName: strings.TrimSpace(item.ToolName),
			})
			if len(item.Children) > 0 {
				walk(item.Children)
			}
		}
	}
	walk(items)
	return events
}

func eventsFromGenericJSON(raw json.RawMessage) []TraceEvent {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return eventsFromGenericValue(value)
}

func eventsFromGenericValue(value any) []TraceEvent {
	switch v := value.(type) {
	case []any:
		events := make([]TraceEvent, 0)
		walkGenericItems(v, &events)
		return events
	case map[string]any:
		if items, ok := anySlice(lookupAny(v, "items", "Items")); ok {
			events := make([]TraceEvent, 0)
			walkGenericItems(items, &events)
			return events
		}
	}
	return nil
}

func walkGenericItems(items []any, events *[]TraceEvent) {
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		kind := stringValue(lookupAny(item, "kind", "Kind"))
		label := stringValue(lookupAny(item, "label", "Label"))
		text := genericItemText(item)
		line := intValue(lookupAny(item, "line", "Line"))
		toolName := stringValue(lookupAny(item, "tool_name", "toolName", "ToolName"))
		*events = append(*events, TraceEvent{
			Index: len(*events),
			Kind:  kind,
			Label: label,
			Text:  text,
			Signals: classifyCandidateItem(
				kind,
				label,
				toolName,
				stringValue(lookupAny(item, "arguments", "Arguments")),
				stringValue(lookupAny(item, "output", "Output")),
				stringValue(lookupAny(item, "text", "Text")),
			),
			Path:     stringValue(lookupAny(item, "path", "Path")),
			Line:     line,
			ToolName: toolName,
		})
		if children, ok := anySlice(lookupAny(item, "children", "Children")); ok {
			walkGenericItems(children, events)
		}
	}
}

func genericItemText(item map[string]any) string {
	parts := make([]string, 0, 5)
	for _, key := range [][]string{
		{"label", "Label"},
		{"tool_name", "toolName", "ToolName"},
		{"arguments", "Arguments"},
		{"output", "Output"},
		{"text", "Text"},
	} {
		if s := stringValue(lookupAny(item, key...)); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func lookupAny(m map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := m[key]; ok {
			return value
		}
	}
	return nil
}

func anySlice(value any) ([]any, bool) {
	items, ok := value.([]any)
	return items, ok
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return ""
	}
}

func intValue(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}

func itemText(item loom.Item) string {
	parts := make([]string, 0, 4)
	if item.Label != "" {
		parts = append(parts, item.Label)
	}
	if item.ToolName != "" {
		parts = append(parts, item.ToolName)
	}
	if item.Arguments != "" {
		parts = append(parts, item.Arguments)
	}
	if item.Output != "" {
		parts = append(parts, item.Output)
	}
	if item.Text != "" {
		parts = append(parts, item.Text)
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func classifyCandidateItem(kind, label, toolName, arguments, output, text string) []Signal {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch {
	case strings.Contains(kind, "user_message"), strings.Contains(kind, "final_answer"):
		return nil
	case strings.Contains(kind, "tool_call"):
		return classifyCandidateText(strings.Join(nonEmpty(label, toolName), "\n"))
	case strings.Contains(kind, "tool_result"):
		// Tool outputs can contain arbitrary page text (including another product's
		// UI labels), so only the visible tool identity participates in trace
		// classification.
		return classifyCandidateText(strings.Join(nonEmpty(label, toolName), "\n"))
	default:
		return classifyCandidateText(strings.Join(nonEmpty(label, toolName, text), "\n"))
	}
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if s := strings.TrimSpace(value); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func summarize(events []TraceEvent) TraceSummary {
	counts := make(map[Signal]int)
	first := make(map[Signal]int)
	rawCounts := make(map[string]int)
	for _, ev := range events {
		rawCounts[ev.Kind]++
		for _, sig := range ev.Signals {
			counts[sig]++
			if _, ok := first[sig]; !ok {
				first[sig] = ev.Index
			}
		}
	}
	return TraceSummary{
		TotalEvents: len(events),
		Counts:      counts,
		FirstIndex:  first,
		Events:      events,
		RawCounts:   rawCounts,
	}
}

func classify(text string) []Signal {
	return classifyText(text, true)
}

func classifyCandidateText(text string) []Signal {
	return classifyText(text, false)
}

func classifyText(text string, broadLongReport bool) []Signal {
	normalized := strings.ToLower(strings.TrimSpace(text))
	if normalized == "" {
		return nil
	}
	added := make(map[Signal]bool)
	add := func(sig Signal) {
		added[sig] = true
	}
	if strings.Contains(normalized, "专家深研") {
		add(SignalExpertResearch)
	}
	if strings.Contains(normalized, "头脑风暴") {
		add(SignalBrainstorm)
	}
	if strings.Contains(normalized, "数据声呐") {
		add(SignalDataSonar)
	}
	if strings.Contains(normalized, "深度阅读") || strings.Contains(normalized, "deep read") {
		add(SignalDeepRead)
	}
	if strings.Contains(normalized, "交叉验证") || strings.Contains(normalized, "多源验证") || strings.Contains(normalized, "多源交叉") {
		add(SignalCrossVerify)
	}
	if strings.Contains(normalized, "start-report") || strings.Contains(normalized, "报告生成工具") || strings.Contains(normalized, "启动最终报告生成") || strings.Contains(normalized, "启动最终报告生成流程") {
		add(SignalStartReport)
	}
	if strings.Contains(normalized, "此报告内容尚未进行可信度核查") {
		add(SignalUnverifiedNotice)
	}
	if strings.Contains(normalized, "点我核查报告") || strings.Contains(normalized, "前往核查") {
		add(SignalVerifyCTA)
	}
	if broadLongReport && strings.Contains(normalized, "万字报告") {
		add(SignalLongReport)
	}
	if !broadLongReport && hasExactLine(text, "万字报告") {
		add(SignalLongReport)
	}
	if strings.Contains(normalized, "citationaudit") ||
		strings.Contains(normalized, "credibilityaudit") ||
		strings.Contains(normalized, "reportrevision") ||
		strings.Contains(normalized, "finalreview") ||
		strings.Contains(normalized, "citation_audit") ||
		strings.Contains(normalized, "credibility_audit") ||
		strings.Contains(normalized, "report_revision") ||
		strings.Contains(normalized, "最终审校") ||
		strings.Contains(normalized, "自动可信度审计") {
		add(SignalForbiddenAudit)
	}
	if len(added) == 0 {
		return nil
	}
	out := make([]Signal, 0, len(added))
	for sig := range added {
		out = append(out, sig)
	}
	slices.Sort(out)
	return out
}

func hasExactLine(text, want string) bool {
	want = strings.TrimSpace(want)
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

func checkCoreOrder(summary TraceSummary) []string {
	requiredOrder := []Signal{
		SignalExpertResearch,
		SignalDataSonar,
		SignalStartReport,
		SignalUnverifiedNotice,
	}
	var issues []string
	for i := 1; i < len(requiredOrder); i++ {
		prev := requiredOrder[i-1]
		next := requiredOrder[i]
		prevIdx, prevOK := summary.FirstIndex[prev]
		nextIdx, nextOK := summary.FirstIndex[next]
		if !prevOK || !nextOK {
			continue
		}
		if prevIdx > nextIdx {
			issues = append(issues, fmt.Sprintf("%s appears after %s (%d > %d)", prev, next, prevIdx, nextIdx))
		}
	}
	return issues
}

func observations(reference, candidate TraceSummary) []string {
	var out []string
	if ref, got := reference.Counts[SignalDataSonar], candidate.Counts[SignalDataSonar]; ref > 0 && got < ref/2 {
		out = append(out, fmt.Sprintf("candidate data_sonar count %d is far below reference %d; real run may be under-searching", got, ref))
	}
	if ref, got := reference.Counts[SignalDeepRead], candidate.Counts[SignalDeepRead]; ref > 0 && got == 0 {
		out = append(out, fmt.Sprintf("reference mentions deep_read %d times, candidate trace has no deep_read signal", ref))
	}
	if ref, got := reference.Counts[SignalCrossVerify], candidate.Counts[SignalCrossVerify]; ref > 0 && got == 0 {
		out = append(out, fmt.Sprintf("reference mentions cross verification %d times, candidate trace has no cross_verify signal", ref))
	}
	if candidate.Counts[SignalVerifyCTA] == 0 && candidate.Counts[SignalUnverifiedNotice] > 0 {
		out = append(out, "candidate records the unverified notice but no separate verify CTA; UI may need to render the CTA from report_verify_status.json")
	}
	return out
}
