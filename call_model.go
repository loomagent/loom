package loom

import (
	"context"
	"errors"
	"fmt"
)

const internalFailoverAttemptLimit uint64 = 8

// CallModelOption 配置一次同步模型调用。
type CallModelOption func(*callModelConfig)

type callModelConfig struct {
	failover        *FailoverConfig
	captureContent  bool
	requestForModel func(ChatModel) ChatRequest
}

// FailoverConfig 参考 Eino 的模型 failover 形态:是否切换、切到哪个模型都由调用方决定。
type FailoverConfig struct {
	ShouldFailover   func(ctx context.Context, attempt FailoverAttempt) bool
	GetFailoverModel func(ctx context.Context, attempt FailoverAttempt) (ChatModel, error)
}

// FailoverAttempt 描述一次已经完成的模型尝试。
type FailoverAttempt struct {
	Attempt  uint64
	Model    ChatModel
	Request  ChatRequest
	Response *ChatResponse
	Error    error
}

// WithModelFailover 为本次 CallModel 启用 failover。
func WithModelFailover(cfg FailoverConfig) CallModelOption {
	return func(c *callModelConfig) {
		c.failover = &cfg
	}
}

// WithCallModelCaptureContent 控制同步调用 span 是否记录 prompt / completion。
func WithCallModelCaptureContent(capture bool) CallModelOption {
	return func(c *callModelConfig) {
		c.captureContent = capture
	}
}

func withCallModelRequestForModel(fn func(ChatModel) ChatRequest) CallModelOption {
	return func(c *callModelConfig) {
		c.requestForModel = fn
	}
}

// ShouldFailoverOnErrorOrFinishReason 返回常见 failover 判定:调用报错或命中特定 FinishReason。
func ShouldFailoverOnErrorOrFinishReason(reasons ...FinishReason) func(context.Context, FailoverAttempt) bool {
	reasonSet := make(map[FinishReason]struct{}, len(reasons))
	for _, reason := range reasons {
		reasonSet[reason] = struct{}{}
	}
	return func(_ context.Context, attempt FailoverAttempt) bool {
		if attempt.Error != nil {
			return true
		}
		if attempt.Response == nil {
			return false
		}
		_, ok := reasonSet[attempt.Response.FinishReason]
		return ok
	}
}

// CallModel 是同步模型调用的统一入口。Retry 仍由 provider 自己处理;这里负责 tracing
// 和 per-call failover。
func CallModel(
	ctx context.Context,
	purpose string,
	model ChatModel,
	req ChatRequest,
	opts ...CallModelOption,
) (*ChatResponse, error) {
	if model == nil {
		return nil, errors.New("loom.CallModel: model 不能为 nil")
	}
	cfg := callModelConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	current := model
	for attemptNum := uint64(1); ; attemptNum++ {
		callReq := req
		if cfg.requestForModel != nil {
			callReq = cfg.requestForModel(current)
		}
		resp, err := callModelOnce(ctx, purpose, current, callReq, cfg.captureContent)
		attempt := FailoverAttempt{
			Attempt:  attemptNum,
			Model:    current,
			Request:  callReq,
			Response: resp,
			Error:    err,
		}
		if !shouldFailover(ctx, cfg.failover, attempt) {
			return resp, err
		}
		if attemptNum >= internalFailoverAttemptLimit {
			if err != nil {
				return resp, fmt.Errorf("loom.CallModel: failover attempt limit reached after %d attempts: %w", attemptNum, err)
			}
			return resp, fmt.Errorf("loom.CallModel: failover attempt limit reached after %d attempts", attemptNum)
		}
		next, failoverErr := cfg.failover.GetFailoverModel(ctx, attempt)
		if failoverErr != nil {
			if err != nil {
				return resp, errors.Join(err, fmt.Errorf("loom.CallModel: get failover model: %w", failoverErr))
			}
			return resp, fmt.Errorf("loom.CallModel: get failover model: %w", failoverErr)
		}
		if next == nil {
			if err != nil {
				return resp, errors.Join(err, errors.New("loom.CallModel: failover model 不能为 nil"))
			}
			return resp, errors.New("loom.CallModel: failover model 不能为 nil")
		}
		current = next
	}
}

func callModelOnce(ctx context.Context, purpose string, model ChatModel, req ChatRequest, captureContent bool) (*ChatResponse, error) {
	llmCtx, span := startLLMSpan(ctx, model, req, purpose, captureContent)
	resp, err := model.Chat(llmCtx, req)
	finalizeLLMSpan(span, resp, captureContent, err)
	return resp, err
}

func shouldFailover(ctx context.Context, cfg *FailoverConfig, attempt FailoverAttempt) bool {
	if cfg == nil || cfg.GetFailoverModel == nil {
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	if cfg.ShouldFailover == nil {
		return attempt.Error != nil
	}
	return cfg.ShouldFailover(ctx, attempt)
}
