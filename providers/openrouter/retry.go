package openrouter

import (
	"context"
	"errors"

	goOpenai "github.com/meguminnnnnnnnn/go-openai"

	"github.com/loomagent/loom"
)

// classifier 把 go-openai 暴露的错误翻成 loom.ErrorClass。
// 任何上层 wrap 过的 error 都通过 errors.As 解出 *goOpenai.APIError /
// *goOpenai.RequestError。
type classifier struct{}

// 编译期断言 — 框架引入新的 ErrorClassifier 字段时编译错暴露。
var _ loom.ErrorClassifier = classifier{}

// ClassifyError 实现 loom.ErrorClassifier。映射与 deepseek classifier 对齐:
//   - 429 / 503             → RateLimit(无限 retry)
//   - 400 / 401 / 402 / 403 / 404 → Permanent(auth / 余额不足 / bad request)
//   - 其它 5xx               → Transient(有限 retry)
//   - 其它 4xx               → Permanent
//   - ctx.Canceled / DeadlineExceeded → Permanent
//   - 其它(net / DNS / TLS) → Transient
func (classifier) ClassifyError(err error) loom.ErrorClass {
	if err == nil {
		return loom.ErrorClassUnknown
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return loom.ErrorClassPermanent
	}
	if status, ok := httpStatusOf(err); ok {
		switch status {
		case 429, 503:
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

// httpStatusOf 从 go-openai 的两种错误类型里解出 HTTP 状态码。
func httpStatusOf(err error) (int, bool) {
	if apiErr, ok := errors.AsType[*goOpenai.APIError](err); ok {
		return apiErr.HTTPStatusCode, true
	}
	if reqErr, ok := errors.AsType[*goOpenai.RequestError](err); ok {
		return reqErr.HTTPStatusCode, true
	}
	return 0, false
}
