package openrouter

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/providers/internal/openaicompat"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"
)

// APIError separates the wire HTTP status from the error body's status. Once
// HTTP 200 is committed, Code carries the provider failure, even in unary calls.
// Cause preserves the original SDK error/event; metadata that can contain user
// input is deliberately excluded from Error(), so routine logs stay diagnostic.
type APIError struct {
	StatusCode   int
	Code         int
	ErrorType    string
	ProviderCode string
	Message      string
	RequestID    string
	Header       http.Header
	Cause        error
	moderation   bool
	raw          jsontext.Value
}

func (e *APIError) Error() string {
	return fmt.Sprintf("openrouter: HTTP %d code=%d error_type=%s provider_code=%s request_id=%s: %s", e.StatusCode, e.Code, e.ErrorType, e.ProviderCode, e.RequestID, e.Message)
}
func (e *APIError) Unwrap() error { return e.Cause }

// RawJSON returns the original error object for opt-in diagnostics. It may
// contain moderation metadata with user input; Error() deliberately omits it.
func (e *APIError) RawJSON() string { return string(e.raw) }
func (e *APIError) Is(target error) bool {
	return target == loom.ErrSensitiveContentRisk && e.moderation
}

func checkFinish(reason string) error {
	if reason == "error" {
		return &APIError{StatusCode: http.StatusOK, Code: http.StatusBadGateway, ErrorType: "unmapped", Message: "inference ended with finish_reason=error"}
	}
	return nil
}

func normalizeError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*APIError](err); ok {
		return err
	}
	if sdk, ok := errors.AsType[*openai.Error](err); ok {
		out := decodeAPIError([]byte(sdk.RawJSON()), sdk.StatusCode, err)
		if out == nil {
			return err
		}
		if sdk.Response != nil {
			out.Header = sdk.Response.Header.Clone()
			out.RequestID = out.Header.Get("X-Generation-ID")
			if out.RequestID == "" {
				out.RequestID = out.Header.Get("X-Request-ID")
			}
		}
		return out
	}
	if raw := openaicompat.StreamErrorBody(err); len(raw) > 0 {
		if out := decodeAPIError(raw, http.StatusOK, err); out != nil {
			if event, ok := errors.AsType[*ssestream.StreamError](err); ok {
				var envelope struct {
					ID string `json:"id"`
				}
				if jsonv2.Unmarshal(event.Event.Data, &envelope) == nil {
					out.RequestID = envelope.ID
				}
			}
			return out
		}
	}
	return err
}

func decodeAPIError(raw []byte, status int, cause error) *APIError {
	var body struct {
		Code     int    `json:"code"`
		Message  string `json:"message"`
		Metadata struct {
			ErrorType    string   `json:"error_type"`
			ProviderCode string   `json:"provider_code"`
			Reasons      []string `json:"reasons"`
			FlaggedInput string   `json:"flagged_input"`
			ProviderName string   `json:"provider_name"`
			ModelSlug    string   `json:"model_slug"`
		} `json:"metadata"`
	}
	if jsonv2.Unmarshal(raw, &body) != nil || body.Code < 400 || body.Code > 599 {
		return nil
	}
	meta := body.Metadata
	// A numeric 403 alone is ambiguous (permissions, budget, guardrails).
	// Typed non-policy evidence takes precedence over legacy moderation fields.
	risk := false
	if body.Code == 403 && (status == 200 || status == 403) {
		switch meta.ErrorType {
		case "content_policy_violation", "refusal":
			risk = true
		case "":
			risk = len(meta.Reasons) > 0 && meta.FlaggedInput != "" && meta.ProviderName != "" && meta.ModelSlug != ""
		}
	}
	return &APIError{StatusCode: status, Code: body.Code, ErrorType: meta.ErrorType, ProviderCode: meta.ProviderCode, Message: body.Message, Cause: cause, moderation: risk, raw: append(jsontext.Value(nil), raw...)}
}

// completionError covers top-level HTTP-200 errors and choice-local failures
// accompanying partial unary output. Neither may become a successful answer.
func completionError(out *openai.ChatCompletion) error {
	if out == nil {
		return nil
	}
	var body struct {
		ID      string         `json:"id"`
		Error   jsontext.Value `json:"error"`
		Choices []struct {
			Error jsontext.Value `json:"error"`
		} `json:"choices"`
	}
	if jsonv2.Unmarshal([]byte(out.RawJSON()), &body) != nil {
		return nil
	}
	decode := func(raw jsontext.Value) error {
		if len(raw) == 0 || string(raw) == "null" {
			return nil
		}
		if err := decodeAPIError(raw, http.StatusOK, nil); err != nil {
			err.RequestID = body.ID
			return err
		}
		return errors.New("openrouter: malformed completion error")
	}
	if err := decode(body.Error); err != nil {
		return err
	}
	for _, choice := range body.Choices {
		if err := decode(choice.Error); err != nil {
			return err
		}
	}
	return nil
}
