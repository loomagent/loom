package proreportbench

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/loomagent/loom"
)

func TestComparePassesUnifuncsCoreTrace(t *testing.T) {
	ref := SummarizeUnifuncs([]Marker{
		{Line: 1, Text: "专家深研 - 07-08 10:21"},
		{Line: 2, Text: "头脑风暴"},
		{Line: 3, Text: "数据声呐 - 07-08 10:21"},
		{Line: 4, Text: "关键事实已经过多源交叉验证。现在调用start-report工具。"},
		{Line: 5, Text: "此报告内容尚未进行可信度核查，您可以 前往核查 让AI替您找茬！"},
	})
	candidate := summarize(eventsFromItems([]loom.Item{
		{Kind: loom.ItemKindStep, Label: "专家深研", Children: []loom.Item{
			{Kind: loom.ItemKindReasoning, Label: "头脑风暴"},
		}},
		{Kind: loom.ItemKindStep, Label: "数据声呐", Children: []loom.Item{
			{Kind: loom.ItemKindReasoning, Label: "覆盖评估", Text: "关键事实已经过多源交叉验证。"},
		}},
		{Kind: loom.ItemKindToolCall, ToolName: "start-report", Label: "start-report"},
		{Kind: loom.ItemKindReasoning, Label: "可信度核查", Text: "此报告内容尚未进行可信度核查，您可以 前往核查 让AI替您找茬！"},
	}))
	report := Compare(ref, candidate)
	if !report.Pass {
		t.Fatalf("Compare pass=false: %+v", report)
	}
}

func TestCompareRejectsForbiddenAuditAndMissingStartReport(t *testing.T) {
	ref := SummarizeUnifuncs([]Marker{
		{Text: "专家深研"},
		{Text: "头脑风暴"},
		{Text: "数据声呐"},
		{Text: "调用start-report"},
		{Text: "此报告内容尚未进行可信度核查，您可以 前往核查 让AI替您找茬！"},
		{Text: "万字报告"},
	})
	candidate := summarize(eventsFromItems([]loom.Item{
		{Kind: loom.ItemKindStep, Label: "专家深研"},
		{Kind: loom.ItemKindStep, Label: "数据声呐"},
		{Kind: loom.ItemKindStep, Label: "CitationAudit + CredibilityAudit"},
		{Kind: loom.ItemKindStep, Label: "万字报告"},
	}))
	report := Compare(ref, candidate)
	if report.Pass {
		t.Fatalf("Compare unexpectedly passed")
	}
	if len(report.Forbidden) != 1 {
		t.Fatalf("forbidden=%d, want 1", len(report.Forbidden))
	}
	if !hasSignal(report.MissingCore, SignalStartReport) {
		t.Fatalf("missing_core=%v, want start_report", report.MissingCore)
	}
}

func TestSummarizeProReportJSONLoadsTurn(t *testing.T) {
	turn := loom.Turn{
		Items: []loom.Item{{
			Kind:  loom.ItemKindStep,
			Label: "专家深研",
			Children: []loom.Item{{
				Kind:  loom.ItemKindReasoning,
				Label: "头脑风暴",
			}},
		}},
	}
	data, err := json.Marshal(turn)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := SummarizeProReportJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("SummarizeProReportJSON: %v", err)
	}
	if summary.Counts[SignalExpertResearch] != 1 || summary.Counts[SignalBrainstorm] != 1 {
		t.Fatalf("counts=%v", summary.Counts)
	}
}

func TestSummarizeProReportJSONLoadsGenericSnakeCaseItems(t *testing.T) {
	data := []byte(`{
		"items": [
			{
				"kind": "step",
				"label": "专家深研",
				"children": [
					{"kind": "reasoning", "label": "头脑风暴"}
				]
			},
			{
				"kind": "tool_call",
				"tool_name": "start-report",
				"arguments": "{}"
			}
		]
	}`)
	summary, err := SummarizeProReportJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("SummarizeProReportJSON: %v", err)
	}
	if summary.Counts[SignalExpertResearch] != 1 || summary.Counts[SignalBrainstorm] != 1 || summary.Counts[SignalStartReport] != 1 {
		t.Fatalf("counts=%v", summary.Counts)
	}
}

func TestSummarizeProReportJSONIgnoresPromptAndToolOutputSignalNoise(t *testing.T) {
	data := []byte(`{
		"Items": [
			{"Kind": "user_message", "Text": "请生成完整万字报告，并进行交叉验证。"},
			{"Kind": "reasoning", "Label": "头脑风暴", "Text": "已解析主题: 请生成完整万字报告，并进行交叉验证。"},
			{"Kind": "tool_result", "Label": "bash", "ToolName": "bash", "Output": "页面内容: 此报告内容尚未进行可信度核查，您可以 前往核查 让AI替您找茬！"},
			{"Kind": "tool_call", "Label": "start-report", "ToolName": "start-report"},
			{"Kind": "reasoning", "Label": "可信度核查", "Text": "此报告内容尚未进行可信度核查，您可以 前往核查 让AI替您找茬！"},
			{"Kind": "reasoning", "Label": "万字报告", "Text": "万字报告"},
			{"Kind": "final_answer", "Text": "正文里也可能提到万字报告这个词。"}
		]
	}`)
	summary, err := SummarizeProReportJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("SummarizeProReportJSON: %v", err)
	}
	if summary.FirstIndex[SignalLongReport] != 5 {
		t.Fatalf("long_report first index = %d, want 5", summary.FirstIndex[SignalLongReport])
	}
	if summary.FirstIndex[SignalUnverifiedNotice] != 4 {
		t.Fatalf("unverified_notice first index = %d, want 4", summary.FirstIndex[SignalUnverifiedNotice])
	}
	if summary.Counts[SignalLongReport] != 1 {
		t.Fatalf("long_report count = %d, want 1", summary.Counts[SignalLongReport])
	}
	if summary.Counts[SignalUnverifiedNotice] != 1 {
		t.Fatalf("unverified_notice count = %d, want 1", summary.Counts[SignalUnverifiedNotice])
	}
}

func hasSignal(values []Signal, needle Signal) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
