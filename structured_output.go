// Structured model calls require complete JSON responses that satisfy the same
// declared contract used for tool arguments, with the model output in place of
// the model input.
package loom

import (
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"strings"
)

const defaultStructuredOutputAttempts uint64 = 2

// maxStructuredOutputNameLen 故意保持无类型常量:它要和 int(b.Len()/len)比较。
// 别加 uint64 显式类型(会编译失败),也别并进上面的 typed 常量组(会触发 SA9004 并被 --fix 错改)。
const maxStructuredOutputNameLen = 64

// StructuredOption 配置 ChatStructuredArgs 的结构化输出和输出重试。
type StructuredOption func(*structuredConfig)

type structuredConfig struct {
	name        string
	description string
	maxAttempts uint64
	validate    func(Args) error
	callOptions []CallModelOption
}

// WithStructuredName 设置传给 provider 的 response_format 名称。默认用契约名。
func WithStructuredName(name string) StructuredOption {
	return func(cfg *structuredConfig) {
		cfg.name = name
	}
}

// WithStructuredDescription 设置传给 provider 和提示词的结构说明。
func WithStructuredDescription(description string) StructuredOption {
	return func(cfg *structuredConfig) {
		cfg.description = description
	}
}

// WithStructuredMaxAttempts 设置输出不满足契约时的最大尝试次数。
func WithStructuredMaxAttempts(maxAttempts uint64) StructuredOption {
	return func(cfg *structuredConfig) {
		cfg.maxAttempts = maxAttempts
	}
}

// WithStructuredValidator 在契约校验后追加业务校验,失败同样触发输出重试。
func WithStructuredValidator(validate func(Args) error) StructuredOption {
	return func(cfg *structuredConfig) {
		cfg.validate = validate
	}
}

// WithStructuredCallOptions 透传 CallModel 选项,如 per-call failover。
func WithStructuredCallOptions(opts ...CallModelOption) StructuredOption {
	return func(cfg *structuredConfig) {
		cfg.callOptions = append(cfg.callOptions, opts...)
	}
}

// WithStructuredFailover 为本次结构化调用启用模型 failover。
func WithStructuredFailover(cfg FailoverConfig) StructuredOption {
	return WithStructuredCallOptions(WithModelFailover(cfg))
}

// StructuredOutputError 表示模型输出不完整、不是合法 JSON，或未通过本地契约 / 业务校验。
type StructuredOutputError struct {
	Attempt uint64
	Content string
	Err     error
}

func (e *StructuredOutputError) Error() string {
	return fmt.Sprintf("structured output attempt %d invalid: %v", e.Attempt, e.Err)
}

func (e *StructuredOutputError) Unwrap() error {
	return e.Err
}

// ChatStructuredArgs 调用模型并把输出按 contract 校验后返回 Args。
//
// 这是工具入参契约的镜像:同一套 ArgsContract 声明字段、约束和描述,只是这里约束的是
// 模型的返回值而不是它的入参。读取方式和工具一样,通过声明时的 typed handle:
//
//	summary := loom.String("summary").MinLen(1).MaxLen(200).Desc("评审摘要")
//	contract := loom.MustArgsContract("review", summary)
//	args, _, err := loom.ChatStructuredArgs(ctx, "review", model, req, contract)
//	summary.Get(args)
//
// Provider 原生支持 json_schema 时会传同一份 schema;仅支持 json_object 时退化成
// JSON object + prompt 约束;本地始终按契约校验。响应必须整体为一个合法 JSON 值。
// 不可映射的业务约束通过 WithStructuredValidator 校验。
func ChatStructuredArgs(
	ctx context.Context,
	purpose string,
	model ChatModel,
	req ChatRequest,
	contract *ArgsContract,
	opts ...StructuredOption,
) (Args, *ChatResponse, error) {
	if model == nil {
		return Args{}, nil, errors.New("loom.ChatStructuredArgs: model 不能为 nil")
	}
	if contract == nil {
		return Args{}, nil, errors.New("loom.ChatStructuredArgs: contract 不能为 nil")
	}

	cfg := structuredConfig{maxAttempts: defaultStructuredOutputAttempts}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.maxAttempts == 0 {
		cfg.maxAttempts = 1
	}
	if cfg.name == "" {
		cfg.name = contract.Name()
	}
	cfg.name = NormalizeStructuredOutputName(cfg.name)
	if cfg.description == "" {
		cfg.description = "structured response"
	}

	// The provider sees a private clone; the contract keeps validating against
	// its own copy, so a provider SDK cannot mutate what DecodeContext enforces.
	schema := contract.Schema()

	var lastResp *ChatResponse
	var lastErr error
	for attempt := uint64(1); attempt <= cfg.maxAttempts; attempt++ {
		callOptions := append([]CallModelOption{}, cfg.callOptions...)
		callOptions = append(callOptions, withCallModelRequestForModel(func(current ChatModel) ChatRequest {
			callReq := withStructuredOutputRequest(req, current.Capabilities(), cfg.name, cfg.description, schema)
			if attempt > 1 {
				callReq.Messages = withStructuredRetryMessages(callReq.Messages, lastResp, lastErr)
			}
			return callReq
		}))
		resp, err := CallModel(ctx, purpose, model, req, callOptions...)
		if err != nil {
			return Args{}, resp, err
		}
		lastResp = resp
		if resp.FinishReason == FinishReasonLength {
			lastErr = &StructuredOutputError{
				Attempt: attempt,
				Content: resp.Content,
				Err:     fmt.Errorf("finish_reason=%s", resp.FinishReason),
			}
			continue
		}
		args, err := contract.DecodeContext(ctx, resp.Content)
		if err == nil && cfg.validate != nil {
			err = cfg.validate(args)
		}
		if err == nil {
			return args, resp, nil
		}
		lastErr = &StructuredOutputError{Attempt: attempt, Content: resp.Content, Err: err}
	}
	return Args{}, lastResp, lastErr
}

// NormalizeStructuredOutputName 生成 provider 可接受的 response_format name。
func NormalizeStructuredOutputName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "structured_output"
	}
	var b strings.Builder
	lastUnderscore := false
	for _, r := range name {
		var write rune
		switch {
		case r == '-' || r == '_':
			write = r
		case r >= 'a' && r <= 'z':
			write = r
		case r >= 'A' && r <= 'Z':
			write = r
		case r >= '0' && r <= '9':
			write = r
		default:
			write = '_'
		}
		if write == '_' {
			if lastUnderscore {
				continue
			}
			lastUnderscore = true
		} else {
			lastUnderscore = false
		}
		b.WriteRune(write)
		if b.Len() >= maxStructuredOutputNameLen {
			break
		}
	}
	out := strings.Trim(b.String(), "_-")
	if out == "" {
		return "structured_output"
	}
	if len(out) > maxStructuredOutputNameLen {
		out = out[:maxStructuredOutputNameLen]
		out = strings.Trim(out, "_-")
	}
	if out == "" {
		return "structured_output"
	}
	return out
}

// StructuredSchemaObject 把 schema 转成普通 JSON object,供 provider SDK 放进 interface{} 字段。
func StructuredSchemaObject(schema *Schema) (map[string]any, error) {
	if schema == nil {
		return nil, errors.New("schema 不能为 nil")
	}
	data, err := jsonv2.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := jsonv2.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func withStructuredOutputRequest(req ChatRequest, caps ModelCapabilities, name, description string, schema *Schema) ChatRequest {
	switch caps.StructuredOutput {
	case StructuredOutputJSONSchema:
		req.ResponseFormat = ResponseFormatDefault
		req.StructuredOutput = &StructuredOutput{
			Mode:        StructuredOutputJSONSchema,
			Name:        name,
			Description: description,
			Schema:      schema,
		}
	case StructuredOutputJSONObject:
		req.ResponseFormat = ResponseFormatJSONObject
		req.StructuredOutput = &StructuredOutput{Mode: StructuredOutputJSONObject}
		req.Messages = appendStructuredPrompt(req.Messages, schema, description)
	case StructuredOutputNone, StructuredOutputUnsupported:
		// 明确不支持 / 能力未声明:都退化为 schema 写进 prompt 的纯文本兜底
		req.ResponseFormat = ResponseFormatDefault
		req.StructuredOutput = nil
		req.Messages = appendStructuredPrompt(req.Messages, schema, description)
	default:
		req.ResponseFormat = ResponseFormatDefault
		req.StructuredOutput = nil
		req.Messages = appendStructuredPrompt(req.Messages, schema, description)
	}
	return req
}

func appendStructuredPrompt(messages []Message, schema *Schema, description string) []Message {
	schemaJSON, err := jsonv2.Marshal(schema, jsontext.WithIndent("  "))
	if err != nil {
		schemaJSON = []byte("{}")
	}
	content := fmt.Sprintf(`请只输出一个满足下列 JSON Schema 的 JSON 值,不要输出 Markdown 或解释。

结构说明:
%s

JSON Schema:
%s`, description, string(schemaJSON))
	out := append([]Message{}, messages...)
	out = append(out, Message{Role: RoleSystem, Content: content})
	return out
}

func withStructuredRetryMessages(messages []Message, resp *ChatResponse, err error) []Message {
	out := append([]Message{}, messages...)
	if resp != nil && strings.TrimSpace(resp.Content) != "" {
		out = append(out, Message{
			Role:    RoleAssistant,
			Content: trimForRetry(resp.Content, 4000),
		})
	}
	out = append(out, Message{
		Role: RoleUser,
		Content: fmt.Sprintf(`上一条输出不满足结构化输出要求: %v

请重新输出完整 JSON。只输出 JSON,不要输出 Markdown 或解释。`, err),
	})
	return out
}

func trimForRetry(s string, limit int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "\n...(truncated)"
}
