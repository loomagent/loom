package ark

import (
	"context"
	"errors"

	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"

	"github.com/loomagent/loom"
)

// classifier turns arkruntime errors into a loom.ErrorClass.
type classifier struct{}

var _ loom.ErrorClassifier = classifier{}

// ClassifyError implements loom.ErrorClassifier.
//
// The source of each error and how it maps, matching the deepseek classifier's meaning and
// covering Ark's *model.APIError and *model.RequestError:
//   - HTTP 429                   → RateLimit, backed off against a shared quota
//   - HTTP 503 / other 5xx       → Transient, retried a bounded number of times
//   - HTTP 401 / 402 / 403 / 400 / 404 → Permanent
//   - other 5xx                  → Transient
//   - other 4xx                  → Permanent
//   - ctx.Canceled / DeadlineExceeded → Permanent, which the framework already covers;
//     this is belt-and-braces
//   - anything else, such as a net, DNS, TLS, or connection-reset failure → Transient
func (classifier) ClassifyError(err error) loom.ErrorClass {
	if err == nil {
		return loom.ErrorClassUnknown
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return loom.ErrorClassPermanent
	}
	if apiErr, ok := errors.AsType[*arkmodel.APIError](err); ok {
		return classifyHTTPStatus(apiErr.HTTPStatusCode)
	}
	if reqErr, ok := errors.AsType[*arkmodel.RequestError](err); ok {
		return classifyHTTPStatus(reqErr.HTTPStatusCode)
	}
	return loom.ErrorClassTransient
}

func classifyHTTPStatus(status int) loom.ErrorClass {
	switch status {
	case 429:
		return loom.ErrorClassRateLimit
	case 400, 401, 402, 403, 404:
		return loom.ErrorClassPermanent
	default:
		if status >= 500 {
			return loom.ErrorClassTransient
		}
		if status >= 400 {
			return loom.ErrorClassPermanent
		}
		return loom.ErrorClassTransient
	}
}

func (classifier) IsServiceUnavailable(err error) bool {
	if apiErr, ok := errors.AsType[*arkmodel.APIError](err); ok {
		return apiErr.HTTPStatusCode == 503
	}
	reqErr, ok := errors.AsType[*arkmodel.RequestError](err)
	return ok && reqErr.HTTPStatusCode == 503
}
