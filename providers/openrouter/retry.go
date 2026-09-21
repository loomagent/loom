package openrouter

import (
	"context"
	"errors"

	"github.com/openai/openai-go/v3"

	"github.com/loomagent/loom"
)

// classifier turns the errors go-openai exposes into a loom.ErrorClass. An error wrapped
// by a caller higher up is unwrapped to an *openai.Error through errors.As.
type classifier struct{}

// Compile-time check, so a new ErrorClassifier field in the framework fails the build.
var _ loom.ErrorClassifier = classifier{}

// ClassifyError implements loom.ErrorClassifier. The mapping matches the deepseek
// classifier:
//   - 429                   → RateLimit, backed off against a shared quota
//   - 503 / other 5xx       → Transient, retried a bounded number of times
//   - 400 / 401 / 402 / 403 / 404 → Permanent: authentication, insufficient balance, bad request
//   - other 5xx             → Transient, retried a bounded number of times
//   - other 4xx             → Permanent
//   - ctx.Canceled / DeadlineExceeded → Permanent
//   - anything else, such as a net, DNS, or TLS failure → Transient
func (classifier) ClassifyError(err error) loom.ErrorClass {
	if err == nil {
		return loom.ErrorClassUnknown
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return loom.ErrorClassPermanent
	}
	if status, ok := httpStatusOf(err); ok {
		switch status {
		case 429:
			return loom.ErrorClassRateLimit
		case 400, 401, 402, 403, 404:
			return loom.ErrorClassPermanent
		}
		if status >= 500 {
			return loom.ErrorClassTransient
		}
		if status >= 400 {
			return loom.ErrorClassPermanent
		}
	}
	return loom.ErrorClassTransient
}

// httpStatusOf extracts the HTTP status code from the official SDK's error types.
func httpStatusOf(err error) (int, bool) {
	if apiErr, ok := errors.AsType[*openai.Error](err); ok {
		return apiErr.StatusCode, true
	}
	return 0, false
}

func (classifier) IsServiceUnavailable(err error) bool {
	status, ok := httpStatusOf(err)
	return ok && status == 503
}
