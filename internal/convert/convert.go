// Package convert 实现三种大模型 API 格式之间的双向转换：
//
//	OpenAI Chat Completions（/v1/chat/completions）
//	OpenAI Responses API（/v1/responses）
//	Anthropic Messages API（/v1/messages）
//
// 设计：以 IR（中间表示）为中心，每种格式实现 Parse/Build 各一对，
// 任意"客户端格式 × 供应商格式"组合（3×3=9 种）都可通过 IR 中转完成转换。
// 同格式请求由代理层直接透传（字节级），不经本包。
package convert

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// 三种 API 格式（同时作为供应商接口类型 api_type 的取值）。
const (
	FormatOpenAI    = "openai"    // OpenAI Chat Completions
	FormatResponses = "responses" // OpenAI Responses API
	FormatAnthropic = "anthropic" // Anthropic Messages API
)

// AnthropicVersion Anthropic API 版本头。
const AnthropicVersion = "2023-06-01"

// NormalizeAPIType 规范化供应商接口类型：空值与未知值回落 openai（兼容旧配置）。
func NormalizeAPIType(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case FormatResponses, "responses_api", "openai_responses", "response":
		return FormatResponses
	case FormatAnthropic, "anthropic_messages", "anthropic_messages_api", "claude":
		return FormatAnthropic
	default:
		return FormatOpenAI
	}
}

// ValidAPIType 判断取值是否合法（空串合法，表示默认 openai）。
func ValidAPIType(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", FormatOpenAI, FormatResponses, FormatAnthropic:
		return true
	}
	return false
}

// APITypes 返回全部合法类型（用于文档/测试）。
func APITypes() []string { return []string{FormatOpenAI, FormatResponses, FormatAnthropic} }

// UpstreamURL 拼接某类型供应商的对话请求 URL。
// anthropic：base 以 /v1 结尾时拼 /messages，否则拼 /v1/messages（同时兼容两种填写习惯）。
func UpstreamURL(apiType, baseURL string) string {
	b := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	switch NormalizeAPIType(apiType) {
	case FormatResponses:
		return b + "/responses"
	case FormatAnthropic:
		if strings.HasSuffix(b, "/v1") {
			return b + "/messages"
		}
		return b + "/v1/messages"
	default:
		return b + "/chat/completions"
	}
}

// ModelsURL 拼接某类型供应商的模型列表 URL。
func ModelsURL(apiType, baseURL string) string {
	b := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if NormalizeAPIType(apiType) == FormatAnthropic && !strings.HasSuffix(b, "/v1") {
		return b + "/v1/models"
	}
	return b + "/models"
}

// SetAuthHeaders 按供应商类型设置上游认证头。
// anthropic 额外携带 x-api-key 与 anthropic-version（Authorization 亦被官方支持，一并携带以最大化兼容）。
func SetAuthHeaders(h http.Header, apiType, key string) {
	if NormalizeAPIType(apiType) == FormatAnthropic {
		h.Set("X-Api-Key", key)
		h.Set("Anthropic-Version", AnthropicVersion)
	}
	h.Set("Authorization", "Bearer "+key)
}

// SetDefaultUserAgent 设置全局默认 User-Agent。
// ua 为空时不设置任何头——保持 Go 标准库默认（Go-http-client/1.1），与旧行为完全一致。
// 必须在应用供应商自定义头之前调用：后者通过 Header.Set 覆盖默认值，
// 最终优先级为「供应商自定义头 > 全局默认 > Go 默认」。
// 背景：部分上游网关（如腾讯元宝 bot 接口）对 Go 默认 UA 直接返回 403「错误请求」，
// 携带 X-Forwarded-For 也无法绕过（实测 2026-10-08：同请求体下 curl UA 200、Go 默认 UA 403）。
func SetDefaultUserAgent(h http.Header, ua string) {
	if ua != "" {
		h.Set("User-Agent", ua)
	}
}

// ===== 请求 IR（中间表示）=====

// ContentPart 多模态内容片段。
type ContentPart struct {
	Type string // text | image
	Text string // type=text

	ImageURL  string // type=image：http(s) 链接
	ImageB64  string // type=image：base64 数据
	ImageMime string // type=image：base64 数据的 MIME 类型
}

// ToolCall 工具调用（assistant 发起）。
type ToolCall struct {
	ID   string
	Name string
	Args string // JSON 对象字符串
}

// InputMessage 输入消息。
type InputMessage struct {
	Role       string // system | user | assistant | tool
	Text       string // 纯文本内容（Parts 为空时使用）
	Parts      []ContentPart
	ToolCalls  []ToolCall // assistant 消息的工具调用
	ToolCallID string     // role=tool 时对应的调用 ID
}

// ToolDef 工具定义。
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolChoice 工具选择策略（归一化）。
// auto=自动；none=不调用；any=必须调用某个（OpenAI required）；tool=指定工具。
type ToolChoice struct {
	Type string
	Name string
}

// RequestIR 请求中间表示。
type RequestIR struct {
	Model           string
	System          string // 系统提示词（CC system 消息 / Responses instructions / Anthropic system）
	Messages        []InputMessage
	MaxTokens       int // 0=未设置
	Temperature     *float64
	TopP            *float64
	Stop            []string
	Stream          bool
	Tools           []ToolDef
	ToolChoice      *ToolChoice
	JSONMode        bool // response_format json_object / text.format json_object
	ReasoningEffort string
}

// ParseRequest 按格式解析请求体为 IR（宽松解析：未知字段忽略）。
func ParseRequest(format string, body []byte) (*RequestIR, error) {
	switch NormalizeAPIType(format) {
	case FormatResponses:
		return parseResponsesRequest(body)
	case FormatAnthropic:
		return parseAnthropicRequest(body)
	default:
		return parseOpenAIRequest(body)
	}
}

// BuildRequest 按格式由 IR 构建请求体；model 为发往上游的真实模型名。
func BuildRequest(format string, ir *RequestIR, model string) ([]byte, error) {
	switch NormalizeAPIType(format) {
	case FormatResponses:
		return buildResponsesRequest(ir, model)
	case FormatAnthropic:
		return buildAnthropicRequest(ir, model)
	default:
		return buildOpenAIRequest(ir, model)
	}
}

// ===== 响应 IR =====

// ResponseIR 响应中间表示。
type ResponseIR struct {
	ID      string
	Model   string
	Created int64 // Unix 秒，0=未知
	Text    string
	// Reasoning 思考/推理内容：OpenAI 系 reasoning_content/reasoning、
	// Anthropic thinking 块、Responses reasoning 条目的统一通道。跨格式转换时
	// 转发到目标格式对应字段；同格式透传不受影响（不经 IR）。
	Reasoning        string
	ToolCalls        []ToolCall
	Finish           string // stop | length | tool_calls | content_filter
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

// ParseResponse 按格式解析上游完整响应体为 IR。
// 上游返回错误对象（非 2xx 时常见）也视为解析失败，错误信息透传。
func ParseResponse(format string, body []byte) (*ResponseIR, error) {
	switch NormalizeAPIType(format) {
	case FormatResponses:
		return parseResponsesResponse(body)
	case FormatAnthropic:
		return parseAnthropicResponse(body)
	default:
		return parseOpenAIResponse(body)
	}
}

// BuildResponse 按客户端格式由 IR 构建完整响应体；model 为响应中的模型名（通常取上游返回值）。
func BuildResponse(format string, ir *ResponseIR, model string) []byte {
	switch NormalizeAPIType(format) {
	case FormatResponses:
		return buildResponsesResponse(ir, model)
	case FormatAnthropic:
		return buildAnthropicResponse(ir, model)
	default:
		return buildOpenAIResponse(ir, model)
	}
}

// ===== 错误输出 =====

// WriteError 按客户端格式输出错误响应：
// openai/responses → OpenAI 风格 {"error":{...}}；anthropic → {"type":"error","error":{...}}。
// 返回实际写给客户端的响应体（供调用日志记录代理 → 客户端响应）。
func WriteError(w http.ResponseWriter, format string, status int, errType, code, message string) []byte {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var body []byte
	switch NormalizeAPIType(format) {
	case FormatAnthropic:
		body, _ = json.Marshal(map[string]interface{}{
			"type":  "error",
			"error": map[string]string{"type": AnthropicErrorType(status), "message": message},
		})
	default:
		body, _ = json.Marshal(map[string]interface{}{
			"error": map[string]string{"message": message, "type": errType, "code": code},
		})
	}
	_, _ = w.Write(body)
	return body
}

// AnthropicErrorType 按 HTTP 状态码映射 Anthropic 错误类型。
func AnthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// ===== usage 提取（三格式统一）=====

// ExtractUsage 从上游完整响应中提取 token 用量。
// stream=false：解析 JSON 对象的 usage 字段；stream=true：解析 SSE 事件流中的 usage
// （OpenAI 末块 usage / Responses response.completed / Anthropic message_start+message_delta）。
// 返回 prompt、completion、total。
func ExtractUsage(body []byte, stream bool) (int64, int64, int64) {
	if len(body) == 0 {
		return 0, 0, 0
	}
	if stream {
		return extractUsageSSE(body)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return 0, 0, 0
	}
	if u, ok := obj["usage"]; ok {
		return usageFromRaw(u)
	}
	return 0, 0, 0
}

// usageFromRaw 兼容 OpenAI（prompt_tokens/completion_tokens/total_tokens）与
// Responses/Anthropic（input_tokens/output_tokens/total_tokens）两种 usage 结构。
func usageFromRaw(raw json.RawMessage) (int64, int64, int64) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, 0, 0
	}
	var u struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
		InputTokens      int64 `json:"input_tokens"`
		OutputTokens     int64 `json:"output_tokens"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return 0, 0, 0
	}
	p, c, t := u.PromptTokens, u.CompletionTokens, u.TotalTokens
	if p == 0 && c == 0 {
		p, c = u.InputTokens, u.OutputTokens
	}
	if t == 0 {
		t = p + c
	}
	return p, c, t
}

// extractUsageSSE 从完整 SSE 流提取 usage（取最后出现的有效值）。
// 必须按事件帧（空行分隔）解析，帧内多行 data: 按规范以 \n 拼接后再解析：
// SSE 规范允许一个事件的载荷分多行 data: 传输，逐行独立解析会导致
// 多行帧全部解析失败、usage 误记 0。
func extractUsageSSE(body []byte) (int64, int64, int64) {
	var p, c, t int64
	var antIn int64                                                    // Anthropic message_start 中的 input_tokens
	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n")) // CRLF 归一化，兼容 \r\n\r\n 事件分隔
	for _, event := range bytes.Split(normalized, []byte("\n\n")) {
		var payload []byte
		for _, line := range bytes.Split(event, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			d := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if len(payload) > 0 {
				payload = append(payload, '\n')
			}
			payload = append(payload, d...)
		}
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var ev map[string]json.RawMessage
		if err := json.Unmarshal(payload, &ev); err != nil {
			continue
		}
		if u, ok := ev["usage"]; ok {
			// OpenAI chunk.usage / Anthropic message_delta.usage
			up, uc, ut := usageFromRaw(u)
			if up > 0 || uc > 0 {
				p, c, t = up, uc, ut
			}
			continue
		}
		if r, ok := ev["response"]; ok {
			// Responses response.completed → data.response.usage
			var robj map[string]json.RawMessage
			if err := json.Unmarshal(r, &robj); err == nil {
				if u, ok := robj["usage"]; ok {
					if up, uc, ut := usageFromRaw(u); up > 0 || uc > 0 {
						p, c, t = up, uc, ut
					}
				}
			}
			continue
		}
		if m, ok := ev["message"]; ok {
			// Anthropic message_start → data.message.usage.input_tokens
			var mobj map[string]json.RawMessage
			if err := json.Unmarshal(m, &mobj); err == nil {
				if u, ok := mobj["usage"]; ok {
					if up, _, _ := usageFromRaw(u); up > 0 {
						antIn = up
					}
				}
			}
		}
	}
	// Anthropic：input 来自 message_start，output 来自 message_delta，合并
	if p == 0 && antIn > 0 {
		p = antIn
		t = p + c
	}
	return p, c, t
}

// ===== 共享工具 =====

// nowUnix 当前 Unix 秒。
func nowUnix() int64 { return time.Now().Unix() }

// nonEmpty 取第一个非空字符串。
func nonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// imageDataURL 生成图片的 URL 表示（data: 内联或 http 链接）。
func imageDataURL(p ContentPart) string {
	if p.ImageB64 != "" {
		mime := p.ImageMime
		if mime == "" {
			mime = "image/png"
		}
		return "data:" + mime + ";base64," + p.ImageB64
	}
	return p.ImageURL
}

// imagePartFromURL 解析 OpenAI 风格 image_url（http 链接或 data: 内联）。
func imagePartFromURL(u string) ContentPart {
	if strings.HasPrefix(u, "data:") {
		rest := strings.TrimPrefix(u, "data:")
		semi := strings.Index(rest, ";")
		if semi > 0 && strings.HasPrefix(rest[semi:], ";base64,") {
			return ContentPart{Type: "image", ImageB64: rest[semi+len(";base64,"):], ImageMime: rest[:semi]}
		}
		return ContentPart{Type: "image", ImageURL: u}
	}
	return ContentPart{Type: "image", ImageURL: u}
}

// rawArgs 校验工具调用参数为合法 JSON，非法时返回空对象。
func rawArgs(args string) json.RawMessage {
	if args == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(args)) {
		return json.RawMessage(args)
	}
	return json.RawMessage("{}")
}

// parseStop 兼容 stop 为字符串或字符串数组。
func parseStop(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s != "" {
			return []string{s}
		}
		return nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	return nil
}

// parseToolChoiceFlat 解析 OpenAI/Responses 风格 tool_choice（字符串或 {"type":"function",...}）。
func parseToolChoiceFlat(raw json.RawMessage) *ToolChoice {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "none":
			return &ToolChoice{Type: "none"}
		case "required":
			return &ToolChoice{Type: "any"}
		case "auto":
			return &ToolChoice{Type: "auto"}
		}
		return nil
	}
	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.Type == "function" {
			if obj.Function != nil {
				return &ToolChoice{Type: "tool", Name: obj.Function.Name}
			}
			if obj.Name != "" {
				return &ToolChoice{Type: "tool", Name: obj.Name}
			}
		}
	}
	return nil
}

// finishToAnthropic IR finish_reason → Anthropic stop_reason。
func finishToAnthropic(f string) string {
	switch f {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

// anthropicToFinish Anthropic stop_reason → IR finish_reason。
func anthropicToFinish(s string) string {
	switch s {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default: // end_turn | stop_sequence
		return "stop"
	}
}

// appendAssistantToolCall 向 IR 追加 assistant 工具调用（与相邻 assistant 消息合并）。
func appendAssistantToolCall(ir *RequestIR, id, name, args string) {
	if n := len(ir.Messages); n > 0 && ir.Messages[n-1].Role == "assistant" {
		ir.Messages[n-1].ToolCalls = append(ir.Messages[n-1].ToolCalls, ToolCall{ID: id, Name: name, Args: args})
		return
	}
	ir.Messages = append(ir.Messages, InputMessage{
		Role:      "assistant",
		ToolCalls: []ToolCall{{ID: id, Name: name, Args: args}},
	})
}

// appendAssistantText 向 IR 追加 assistant 文本（与相邻 assistant 消息合并）。
// 不区分上一条是否已含 ToolCalls：同一 assistant 回合中"先工具调用、后文本"（如 Codex
// 回放历史）也必须合并为一条消息，否则拆分后 tool 结果消息不再紧跟发起调用的
// assistant，违反 Chat Completions 语义（tool 响应须紧随含 tool_calls 的 assistant），
// 校验严格的上游会直接 400 invalid params。
func appendAssistantText(ir *RequestIR, text string, parts []ContentPart) {
	if n := len(ir.Messages); n > 0 && ir.Messages[n-1].Role == "assistant" {
		ir.Messages[n-1].Text += text
		ir.Messages[n-1].Parts = append(ir.Messages[n-1].Parts, parts...)
		return
	}
	ir.Messages = append(ir.Messages, InputMessage{Role: "assistant", Text: text, Parts: parts})
}

// openAIErrorMessage 提取 OpenAI 风格错误体的 message。
func openAIErrorMessage(body []byte) string {
	var e struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err == nil && e.Error != nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return "unrecognized upstream response"
}
