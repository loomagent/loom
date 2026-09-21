package deepseek

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"

	"github.com/loomagent/loom"
)

// classifier turns the errors the official SDK exposes into a loom.ErrorClass. An error
// wrapped by a caller higher up is unwrapped to an *openai.Error through errors.As.
type classifier struct{}

// Compile-time check, so a new ErrorClassifier field in the framework fails the build.
var _ loom.ErrorClassifier = classifier{}

// ClassifyError implements loom.ErrorClassifier.
//
// The source of each error and how it maps:
//   - *openai.Error for a non-2xx HTTP status:
//   - 429                   → RateLimit, bounded by the shared cooldown and elapsed budget
//   - 503 and other 5xx     → Transient, retried a bounded number of times
//   - 401, 402, 403, 400    → Permanent: authentication, insufficient balance, bad request
//   - any other 4xx         → Permanent: a bad argument, a missing model
//   - ctx.Canceled or DeadlineExceeded → Permanent, which classifyForBackoff already
//     covers; this is belt-and-braces
//   - anything else, such as a net, DNS, or TLS handshake failure → Transient
func (classifier) ClassifyError(err error) loom.ErrorClass {
	if err == nil {
		return loom.ErrorClassUnknown
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return loom.ErrorClassPermanent
	}
	if errors.Is(err, loom.ErrSensitiveContentRisk) {
		return loom.ErrorClassPermanent
	}
	if apiErr, ok := errors.AsType[*openai.Error](err); ok {
		switch apiErr.StatusCode {
		case http.StatusTooManyRequests:
			return loom.ErrorClassRateLimit
		case 400, 401, 402, 403, 404:
			return loom.ErrorClassPermanent
		}
		if apiErr.StatusCode >= 500 {
			return loom.ErrorClassTransient
		}
		// Any other 4xx is permanent: a client error that retrying cannot fix
		if apiErr.StatusCode >= 400 {
			return loom.ErrorClassPermanent
		}
	}
	// Network-layer errors default to Transient: connection reset, DNS, a TLS handshake
	return loom.ErrorClassTransient
}

// RetryAfter extracts DeepSeek's response header for shared credential-level
// cooldown. Both delta-seconds and HTTP-date forms are accepted per RFC 9110.
func (classifier) RetryAfter(err error) time.Duration {
	apiErr, ok := errors.AsType[*openai.Error](err)
	if !ok || apiErr.Response == nil {
		return 0
	}
	value := strings.TrimSpace(apiErr.Response.Header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil {
		return max(time.Duration(0), time.Duration(seconds)*time.Second)
	}
	when, parseErr := http.ParseTime(value)
	if parseErr != nil {
		return 0
	}
	return max(time.Duration(0), time.Until(when))
}

func (classifier) IsServiceUnavailable(err error) bool {
	apiErr, ok := errors.AsType[*openai.Error](err)
	return ok && apiErr.StatusCode == http.StatusServiceUnavailable
}
