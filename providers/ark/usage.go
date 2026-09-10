package ark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	arkutils "github.com/volcengine/volcengine-go-sdk/service/arkruntime/utils"
)

// The SDK uses value integers and loses the difference between absent, null
// and explicit zero. Capture presence before SDK decoding, scoped to one call
// or one SSE frame; never infer it from a model name or from reasoning text.
type usageEvidence struct {
	Usage *struct {
		CompletionTokensDetails *struct {
			ReasoningTokens *uint64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

func (e *usageEvidence) known() bool {
	return e != nil && e.Usage != nil && e.Usage.CompletionTokensDetails != nil && e.Usage.CompletionTokensDetails.ReasoningTokens != nil
}

type usageEvidenceKey struct{}

type usageTransport struct{ base http.RoundTripper }

func (t usageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	evidence, capture := req.Context().Value(usageEvidenceKey{}).(*usageEvidence)
	if capture {
		*evidence = usageEvidence{}
	} // retries must not inherit a prior attempt
	resp, err := t.base.RoundTrip(req)
	if err != nil || !capture || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, err
	}
	// Preserve the SDK's request, error and response handling. Only unary calls
	// opt in; streaming bodies continue to be consumed incrementally by the SDK.
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	if err := json.Unmarshal(raw, evidence); err != nil {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("ark usage evidence: %w", err)
	}
	return resp, nil
}

func withUsageTransport(apiKey string, opts []arkruntime.ConfigOption) []arkruntime.ConfigOption {
	cfg := arkruntime.NewClientConfig(apiKey, "", "", opts...)
	client := *cfg.HTTPClient
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Transport = usageTransport{base: base}
	return append(opts, arkruntime.WithHTTPClient(&client))
}

func captureUsage(ctx context.Context) (context.Context, *usageEvidence) {
	evidence := new(usageEvidence)
	return context.WithValue(ctx, usageEvidenceKey{}, evidence), evidence
}

type usageUnmarshaler struct {
	inner    arkutils.Unmarshaler
	evidence usageEvidence
}

func (u *usageUnmarshaler) Unmarshal(raw []byte, value interface{}) error {
	u.evidence = usageEvidence{} // usage can disappear again in the next frame
	if err := u.inner.Unmarshal(raw, value); err != nil {
		return err
	}
	return json.Unmarshal(raw, &u.evidence)
}
