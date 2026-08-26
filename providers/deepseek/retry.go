package deepseek

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	goseek "github.com/storynap/goseek"

	"github.com/loomagent/loom"
)

// classifier 把 goseek 暴露的错误翻成 loom.ErrorClass。
// 任何上层 wrap 过的 error 都通过 errors.As 解出 *goseek.APIError。
type classifier struct{}

// 编译期断言 — 框架引入新的 ErrorClassifier 字段时编译错暴露。
var _ loom.ErrorClassifier = classifier{}

// ClassifyError 实现 loom.ErrorClassifier。
//
// 错误来源 + 映射:
//   - *goseek.APIError(HTTP 非 2xx):
//   - 429                   → RateLimit(受共享 cooldown 和 elapsed budget 约束)
//   - 503 / 其它 5xx         → Transient(有限 retry)
//   - 401 / 402 / 403 / 400 → Permanent(auth / 余额不足 / bad request)
//   - 5xx (其它)             → Transient(有限 retry)
//   - 4xx (其它)             → Permanent(参数错 / 模型不存在等)
//   - ctx.Canceled / DeadlineExceeded → Permanent(框架已在 classifyForBackoff
//     兜底,这里 redundant 保险)
//   - 其它(net / DNS / TLS handshake 失败等)→ Transient
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
	if apiErr, ok := errors.AsType[*goseek.APIError](err); ok {
		switch apiErr.StatusCode {
		case http.StatusTooManyRequests:
			return loom.ErrorClassRateLimit
		case 400, 401, 402, 403, 404:
			return loom.ErrorClassPermanent
		}
		if apiErr.StatusCode >= 500 {
			return loom.ErrorClassTransient
		}
		// 其它 4xx → permanent(client 错误,retry 无用)
		if apiErr.StatusCode >= 400 {
			return loom.ErrorClassPermanent
		}
	}
	// 默认网络层错误归为 Transient(connection reset / DNS / TLS 握手 等)
	return loom.ErrorClassTransient
}

// RetryAfter extracts DeepSeek's response header for shared credential-level
// cooldown. Both delta-seconds and HTTP-date forms are accepted per RFC 9110.
func (classifier) RetryAfter(err error) time.Duration {
	apiErr, ok := errors.AsType[*goseek.APIError](err)
	if !ok {
		return 0
	}
	value := strings.TrimSpace(apiErr.Header.Get("Retry-After"))
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
	apiErr, ok := errors.AsType[*goseek.APIError](err)
	return ok && apiErr.StatusCode == http.StatusServiceUnavailable
}
