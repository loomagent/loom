package zhipuai

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/loomagent/loom"
	"github.com/openai/openai-go/v3"
)

// APIError keeps Zhipu's business code separate from HTTP status: a 429 can
// mean exhausted balance, not a transient rate limit. Cause remains unwrap-able.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	RequestID  string
	Header     http.Header
	Cause      error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("zhipuai: HTTP %d code=%s request_id=%s: %s", e.StatusCode, e.Code, e.RequestID, e.Message)
}
func (e *APIError) Unwrap() error { return e.Cause }
func (e *APIError) Is(target error) bool {
	return target == loom.ErrSensitiveContentRisk && e.Code == "1301"
}

// RejectsCapability requires both a parameter error code and an explicit
// reference to the probed field. Other 400s are operational failures.
func (e *APIError) RejectsCapability(field string) bool {
	if e.StatusCode != 400 {
		return false
	}
	switch e.Code {
	case "1210", "1213", "1214", "1215":
	default:
		return false
	}
	msg := strings.ToLower(e.Message)
	fieldNames := []string{strings.ToLower(field)}
	switch field {
	case "thinking":
		fieldNames = append(fieldNames, "思考", "推理开关")
	case "reasoning_effort":
		fieldNames = append(fieldNames, "推理强度", "思考强度")
	case "response_format":
		fieldNames = append(fieldNames, "响应格式", "输出格式")
	}
	matched := false
	for _, name := range fieldNames {
		matched = matched || strings.Contains(msg, name)
	}
	// GLM-5.3 also rejects unsupported effort values with the same localized
	// thinking error, but explicitly lists the allowed effort values.
	if field == "reasoning_effort" && strings.Contains(msg, "low") && strings.Contains(msg, "high") && strings.Contains(msg, "max") && (strings.Contains(msg, "请使用") || strings.Contains(msg, "仅支持") || strings.Contains(msg, "only support")) {
		matched = true
	}
	if !matched {
		return false
	}
	markers := []string{"not support", "unsupported", "不支持", "only support", "仅支持"}
	if field == "response_format" {
		// An invalid schema/parameter is not evidence that the format itself is
		// unavailable. Require an explicit capability rejection for this probe.
		markers = append(markers, "type is unavailable", "type unavailable")
	} else {
		markers = append(markers, "invalid", "非法")
	}
	for _, marker := range markers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func normalizeError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*APIError](err); ok {
		return err
	}
	if e, ok := errors.AsType[*openai.Error](err); ok {
		out := &APIError{StatusCode: e.StatusCode, Code: e.Code, Message: e.Message, Cause: err}
		// SDK Code is a string; tolerate numeric business codes in compatible responses.
		var raw struct {
			Code      any    `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		}
		if jsonv2.Unmarshal([]byte(e.RawJSON()), &raw) == nil {
			if raw.Code != nil {
				out.Code = fmt.Sprint(raw.Code)
			}
			if raw.Message != "" {
				out.Message = raw.Message
			}
			out.RequestID = raw.RequestID
		}
		if e.Response != nil {
			out.Header = e.Response.Header.Clone()
			if out.RequestID == "" {
				out.RequestID = out.Header.Get("X-Request-ID")
			}
		}
		return out
	}
	return err
}

type classifier struct{}

var _ loom.ErrorClassifier = classifier{}

func (classifier) ClassifyError(err error) loom.ErrorClass {
	if err == nil {
		return loom.ErrorClassUnknown
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, loom.ErrSensitiveContentRisk) {
		return loom.ErrorClassPermanent
	}
	if e, ok := errors.AsType[*APIError](err); ok {
		switch e.Code {
		case "1302":
			return loom.ErrorClassRateLimit
		case "1305", "1200", "1230", "1234":
			return loom.ErrorClassTransient
		case "1000", "1001", "1003", "1005", "1113", "1210", "1211", "1212", "1213", "1214", "1215", "1220", "1221", "1222", "1261", "1301",
			"1308", "1309", "1310", "1311", "1313", "1314", "1315", "1316", "1317", "1318", "1319", "1320", "1321",
			"model_context_window_exceeded", "invalid_finish_reason":
			return loom.ErrorClassPermanent
		}
		// Unknown 429s receive only the finite transient budget; never a long
		// rate-limit loop for an undocumented business/quota error.
		if e.StatusCode == 429 || e.StatusCode >= 500 {
			return loom.ErrorClassTransient
		}
		return loom.ErrorClassPermanent
	}
	return loom.ErrorClassTransient
}
func (classifier) IsServiceUnavailable(err error) bool {
	e, ok := errors.AsType[*APIError](err)
	return ok && (e.Code == "1305" || e.StatusCode == 503)
}
func (classifier) RetryAfter(err error) time.Duration {
	e, ok := errors.AsType[*APIError](err)
	if !ok {
		return 0
	}
	value := strings.TrimSpace(e.Header.Get("Retry-After"))
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		// Bound invalid/overflowing headers; the Loom elapsed budget still applies.
		if seconds <= 0 {
			return 0
		}
		if seconds > 86400 {
			seconds = 86400
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(time.Duration(0), time.Until(when))
	}
	return 0
}
