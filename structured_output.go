package loom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

const defaultStructuredOutputAttempts uint64 = 2

// maxStructuredOutputNameLen 故意保持无类型常量:它要和 int(b.Len()/len)比较。
// 别加 uint64 显式类型(会编译失败),也别并进上面的 typed 常量组(会触发 SA9004 并被 --fix 错改)。
const maxStructuredOutputNameLen = 64

// StructuredChatOption 配置 ChatStructured 的结构化输出和输出重试。
type StructuredChatOption[T any] func(*structuredChatConfig[T])

type structuredChatConfig[T any] struct {
	name        string
	description string
	maxAttempts uint64
	validate    func(T) error
	callOptions []CallModelOption
}

// WithStructuredName 设置传给 provider 的 response_format 名称。
func WithStructuredName[T any](name string) StructuredChatOption[T] {
	return func(cfg *structuredChatConfig[T]) {
		cfg.name = name
	}
}

// WithStructuredDescription 设置传给 provider 和提示词的结构说明。
func WithStructuredDescription[T any](description string) StructuredChatOption[T] {
	return func(cfg *structuredChatConfig[T]) {
		cfg.description = description
	}
}

// WithStructuredMaxAttempts 设置输出不满足 schema 时的最大尝试次数。
func WithStructuredMaxAttempts[T any](maxAttempts uint64) StructuredChatOption[T] {
	return func(cfg *structuredChatConfig[T]) {
		cfg.maxAttempts = maxAttempts
	}
}

// WithStructuredValidator 在 schema 校验后追加业务校验,失败同样触发输出重试。
func WithStructuredValidator[T any](validate func(T) error) StructuredChatOption[T] {
	return func(cfg *structuredChatConfig[T]) {
		cfg.validate = validate
	}
}

// WithStructuredCallOptions 透传 CallModel 选项,如 per-call failover。
func WithStructuredCallOptions[T any](opts ...CallModelOption) StructuredChatOption[T] {
	return func(cfg *structuredChatConfig[T]) {
		cfg.callOptions = append(cfg.callOptions, opts...)
	}
}

// WithStructuredFailover 为本次结构化调用启用模型 failover。
func WithStructuredFailover[T any](cfg FailoverConfig) StructuredChatOption[T] {
	return WithStructuredCallOptions[T](WithModelFailover(cfg))
}

// StructuredOutputError 表示模型返回了 JSON,但没有满足本地 schema 或业务校验。
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

// ChatStructured 调用模型并把输出解析为 T。
//
// Schema 从 T 自动生成。Provider 原生支持 json_schema 时会传 schema;仅支持
// json_object 时退化成 JSON object + prompt 约束;本地始终执行 parse + schema validate。
func ChatStructured[T any](
	ctx context.Context,
	purpose string,
	model ChatModel,
	req ChatRequest,
	opts ...StructuredChatOption[T],
) (T, *ChatResponse, error) {
	var zero T
	if model == nil {
		return zero, nil, errors.New("loom.ChatStructured: model 不能为 nil")
	}

	cfg := structuredChatConfig[T]{maxAttempts: defaultStructuredOutputAttempts}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.maxAttempts == 0 {
		cfg.maxAttempts = 1
	}
	if cfg.name == "" {
		cfg.name = defaultStructuredOutputName[T]()
	}
	cfg.name = NormalizeStructuredOutputName(cfg.name)
	if cfg.description == "" {
		cfg.description = "structured response"
	}

	schema, err := jsonschema.For[T](nil)
	if err != nil {
		return zero, nil, fmt.Errorf("loom.ChatStructured: 生成 JSON schema: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return zero, nil, fmt.Errorf("loom.ChatStructured: 解析 JSON schema: %w", err)
	}

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
			return zero, resp, err
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
		value, err := parseStructuredResponse[T](resp.Content, resolved, cfg.validate)
		if err == nil {
			return value, resp, nil
		}
		lastErr = &StructuredOutputError{Attempt: attempt, Content: resp.Content, Err: err}
	}
	return zero, lastResp, lastErr
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
func StructuredSchemaObject(schema *jsonschema.Schema) (map[string]any, error) {
	if schema == nil {
		return nil, errors.New("schema 不能为 nil")
	}
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func withStructuredOutputRequest(req ChatRequest, caps ModelCapabilities, name, description string, schema *jsonschema.Schema) ChatRequest {
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

func appendStructuredPrompt(messages []Message, schema *jsonschema.Schema, description string) []Message {
	schemaJSON, err := json.MarshalIndent(schema, "", "  ")
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

func parseStructuredResponse[T any](content string, schema *jsonschema.Resolved, validate func(T) error) (T, error) {
	var zero T
	raw, err := extractJSONValue(content)
	if err != nil {
		return zero, err
	}
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		return zero, fmt.Errorf("解析 JSON: %w", err)
	}
	if err := schema.Validate(instance); err != nil {
		return zero, fmt.Errorf("校验 JSON schema: %w", err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, fmt.Errorf("反序列化结构体: %w", err)
	}
	if validate != nil {
		if err := validate(out); err != nil {
			return zero, fmt.Errorf("业务校验: %w", err)
		}
	}
	return out, nil
}

func extractJSONValue(content string) ([]byte, error) {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, errors.New("输出为空")
	}
	if json.Valid([]byte(content)) {
		return []byte(content), nil
	}
	startObj := strings.Index(content, "{")
	startArr := strings.Index(content, "[")
	start := -1
	end := -1
	if startObj >= 0 && (startArr < 0 || startObj < startArr) {
		start = startObj
		end = strings.LastIndex(content, "}")
	} else if startArr >= 0 {
		start = startArr
		end = strings.LastIndex(content, "]")
	}
	if start < 0 || end <= start {
		return nil, errors.New("未找到 JSON 值")
	}
	raw := strings.TrimSpace(content[start : end+1])
	if !json.Valid([]byte(raw)) {
		return nil, errors.New("提取到的 JSON 无效")
	}
	return []byte(raw), nil
}

func defaultStructuredOutputName[T any]() string {
	t := reflect.TypeFor[T]()
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Name() == "" {
		return "structured_output"
	}
	return t.Name()
}

func trimForRetry(s string, limit int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "\n...(truncated)"
}
