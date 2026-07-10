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
// 错误来源 + 映射(跟 deepseek classifier 同语义,但用 ark 的 *model.RequestError):
//   - HTTP 429 / 503             → RateLimit(无限 retry)
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
	var reqErr *arkmodel.RequestError
	if errors.As(err, &reqErr) {
		switch reqErr.HTTPStatusCode {
		case 429, 503:
			return loom.ErrorClassRateLimit
		case 400, 401, 402, 403, 404:
			return loom.ErrorClassPermanent
		}
		if reqErr.HTTPStatusCode >= 500 {
			return loom.ErrorClassTransient
		}
		if reqErr.HTTPStatusCode >= 400 {
			return loom.ErrorClassPermanent
		}
	}
	return loom.ErrorClassTransient
}
