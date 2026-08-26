package ark

import (
	"context"
	"errors"

	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"

	"github.com/loomagent/loom"
)

// classifier 把 arkruntime 错误翻成 loom.ErrorClass。
type classifier struct{}

var _ loom.ErrorClassifier = classifier{}

// ClassifyError 实现 loom.ErrorClassifier。
//
// 错误来源 + 映射(跟 deepseek classifier 同语义,覆盖 ark 的
// *model.APIError / *model.RequestError):
//   - HTTP 429                   → RateLimit(共享额度退避)
//   - HTTP 503 / 其它 5xx         → Transient(有限 retry)
//   - HTTP 401 / 402 / 403 / 400 / 404 → Permanent
//   - HTTP 5xx (其它)              → Transient
//   - HTTP 4xx (其它)              → Permanent
//   - ctx.Canceled / DeadlineExceeded → Permanent(框架已 兜底,redundant 保险)
//   - 其它(net / DNS / TLS / 连接重置等)→ Transient
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
