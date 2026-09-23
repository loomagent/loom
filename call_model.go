package loom

import (
	"context"
	"errors"
	"fmt"
)

const internalFailoverAttemptLimit uint64 = 8

// CallModelOption configures one synchronous model call.
type CallModelOption func(*callModelConfig)

type callModelConfig struct {
	failover        *FailoverConfig
	captureContent  bool
	requestForModel func(ChatModel) (ChatRequest, error)
}

// FailoverConfig follows Eino's failover shape: the caller decides whether to
// switch and which model to switch to.
type FailoverConfig struct {
	ShouldFailover   func(ctx context.Context, attempt FailoverAttempt) bool
	GetFailoverModel func(ctx context.Context, attempt FailoverAttempt) (ChatModel, error)
}

// FailoverAttempt describes one model attempt that has already finished.
type FailoverAttempt struct {
	Attempt  uint64
	Model    ChatModel
	Request  ChatRequest
	Response *ChatResponse
	Error    error
}

// WithModelFailover enables failover for this CallModel.
func WithModelFailover(cfg FailoverConfig) CallModelOption {
	return func(c *callModelConfig) {
		c.failover = &cfg
	}
}

// WithCallModelCaptureContent controls whether a synchronous call's span records
// the prompt and the completion.
func WithCallModelCaptureContent(capture bool) CallModelOption {
	return func(c *callModelConfig) {
		c.captureContent = capture
	}
}

func withCallModelRequestForModel(fn func(ChatModel) (ChatRequest, error)) CallModelOption {
	return func(c *callModelConfig) {
		c.requestForModel = fn
	}
}

// ShouldFailoverOnErrorOrFinishReason is the common failover rule: switch when the
// call errors or the finish reason is one of those listed.
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

// CallModel is the single entry point for synchronous model calls. Providers still
// own retries; this handles tracing and per-call failover.
func CallModel(
	ctx context.Context,
	purpose string,
	model ChatModel,
	req ChatRequest,
	opts ...CallModelOption,
) (*ChatResponse, error) {
	if model == nil {
		return nil, errors.New("loom.CallModel: model must not be nil")
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
			built, err := cfg.requestForModel(current)
			if err != nil {
				return nil, err
			}
			callReq = built
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
				return resp, errors.Join(err, errors.New("loom.CallModel: failover model must not be nil"))
			}
			return resp, errors.New("loom.CallModel: failover model must not be nil")
		}
		current = next
	}
}

func callModelOnce(ctx context.Context, purpose string, model ChatModel, req ChatRequest, captureContent bool) (*ChatResponse, error) {
	llmCtx, span := startLLMSpan(ctx, model, req, purpose, captureContent)
	resp, err := model.Chat(llmCtx, req)
	finalizeLLMSpan(span, resp, captureContent, err)
	if resp != nil && nonZeroUsage(resp.Usage) {
		if scope := usageScopeFromContext(ctx); scope != nil {
			modelID := resp.Model
			if modelID == "" {
				modelID = model.Name()
			}
			scope.state.emitLLMCalled(ctx, scope, modelID, purpose, resp.Usage)
		}
	}
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

// WithCallModelRequestForModel builds the request for each selected model,
// including failover models. Hosts use it to select a declared response format
// without sending one provider's capability assumptions to another provider.
func WithCallModelRequestForModel(build func(ChatModel) (ChatRequest, error)) CallModelOption {
	return withCallModelRequestForModel(build)
}
