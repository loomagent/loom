package modelprobe

import (
	"context"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/providers/ark"
)

func TestArkProbeRecordsWireAliasWithoutClaimingNativeIndependence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.UnmarshalRead(r.Body, &req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if req["model"] != "deepseek-v4-pro-ga-260813" {
			t.Errorf("model=%v", req["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"deepseek-v4-pro-ga-260813","choices":[{"index":0,"message":{"role":"assistant","content":"{\"ok\":true}","reasoning_content":"reason"},"finish_reason":"stop"}],"usage":{"completion_tokens_details":{"reasoning_tokens":3}}}`))
	}))
	defer server.Close()
	report, err := Probe(context.Background(), BuilderFunc(func(_ context.Context, caps loom.ModelCapabilities) (loom.ChatModel, error) {
		return ark.New(ark.Config{APIKey: "test", ModelName: "deepseek-v4-pro-ga-260813", BaseURL: server.URL, Capabilities: &caps})
	}), Options{ReasoningEfforts: []loom.ReasoningEffort{"low", "high", "max", "medium"}})
	if err != nil {
		t.Fatal(err)
	}
	c := report.EffortCoverage
	if !c.Complete || c.NativeIndependenceProven || c.Contract.Aliases["medium"] != "low" || c.CandidateSource != "explicit_candidates" {
		t.Fatalf("coverage=%+v", c)
	}
	if report.Checks[1].Outcome != OutcomeNegative {
		t.Fatalf("ignored disable counted as success: %+v", report.Checks[1])
	}
	for _, check := range report.Checks {
		e := check.Evidence
		if !e.SentParametersKnown || e.Acceptance != "accepted" {
			t.Fatalf("missing wire evidence: %+v", check)
		}
		if check.Name == CheckReasoningDefault && len(e.SentParameters) != 0 {
			t.Fatalf("default probe sent controls: %+v", check)
		}
		if check.Effort != "" && e.SentParameters["reasoning_effort"] != string(check.Effort) {
			t.Fatalf("effort mapped locally: %+v", check)
		}
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip Report
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.EffortCoverage.NativeIndependenceProven || roundTrip.EffortCoverage.Contract.Aliases["medium"] != "low" {
		t.Fatalf("lost semantics: %+v", roundTrip)
	}
}
