// stream.go：跨格式流式转换（增量）。
//
// 代理对流式请求（stream=true）边收边转发：同格式上游 SSE 原样透传（代理层处理，不经本包）；
// 跨格式时由本包把上游 SSE 事件流增量转换为目标客户端格式的 SSE 事件流。
//
// 设计：三段式流水线
//
//	上游 data payload --ParseStreamPayload--> 归一化 StreamEvent --StreamEmitter--> 客户端 SSE 帧
//
// 每种上游格式实现一个解析器、每种客户端格式实现一个发射器，
// 任意组合（3 上游 × 3 客户端）都可转换。StreamConverter 组合两者，
// 内部维护 SSE 帧扫描、工具索引映射、usage 累积与收尾帧合成。
package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ===== 归一化增量事件 =====

// StreamEvent 归一化增量事件（跨格式的流式中间表示）。
type StreamEvent struct {
	Kind string // start | text | thinking | tool_start | tool_args | tool_end | finish | usage | done | error
	Text string // text：增量文本；thinking：思考增量；error：错误消息

	ToolIndex int    // tool_*：上游侧工具索引
	ToolID    string // tool_start：工具调用 ID
	ToolName  string // tool_start：工具名
	ToolArgs  string // tool_args：参数增量

	Finish string // finish：stop | length | tool_calls | content_filter

	Prompt     int64 // usage：prompt/input tokens
	Completion int64 // usage：completion/output tokens
	Total      int64 // usage：total tokens

	ID    string // start：上游响应 ID（可选）
	Model string // start：上游模型名（可选）
}

const (
	evStart     = "start"
	evText      = "text"
	evThinking  = "thinking" // v1.8.4（M6）：思考/推理内容增量（Anthropic thinking / OpenAI reasoning_content / Responses reasoning）
	evToolStart = "tool_start"
	evToolArgs  = "tool_args"
	evToolEnd   = "tool_end"
	evFinish    = "finish"
	evUsage     = "usage"
	evDone      = "done"
	evError     = "error"
)

// UpstreamStreamError 上游在 200 流式响应中发送的错误事件（如 OpenAI 兼容网关的
// "200 + data: {"error":...}" 延迟报错）。首事件阶段出现时可重试换 Key/供应商。
type UpstreamStreamError struct {
	Message string
}

func (e *UpstreamStreamError) Error() string { return "上游流式响应错误: " + e.Message }

// ===== SSE 帧扫描器 =====

// sseScanner 从增量字节流中提取完整 SSE 事件的 data 载荷。
// 事件以空行（\n\n 或 \r\n\r\n）分隔；同一事件内多行 data: 按规范以 \n 拼接；
// event:/id:/retry: 等行忽略（载荷自带类型信息，三格式均可从 JSON 判断）。
type sseScanner struct {
	buf []byte
}

// maxScannerBuffer 帧缓冲上限（16MB）：防御故障/恶意上游持续发送
// 永不以空行结尾的字节流导致缓冲无界增长（L12 修复；代理层对响应副本的
// 64MB 上限不约束喂入扫描器的字节）。
const maxScannerBuffer = 16 << 20

// feed 追加字节并返回新完成的 data 载荷（不含 [DONE]，[DONE] 由调用方按需处理）。
func (s *sseScanner) feed(chunk []byte) []string {
	s.buf = append(s.buf, chunk...)
	var out []string
	for {
		idx, sepLen := sseEventEnd(s.buf)
		if idx < 0 {
			break
		}
		event := s.buf[:idx]
		s.buf = s.buf[idx+sepLen:]
		if payload := sseDataPayload(event); payload != "" {
			out = append(out, payload)
		}
	}
	// 溢出保护：超限时保留尾部一半缓冲——该流必然已非规范，
	// 丢弃旧字节优于 OOM，且后续若出现空行仍可解析最新事件
	if len(s.buf) > maxScannerBuffer {
		s.buf = s.buf[len(s.buf)-maxScannerBuffer/2:]
	}
	return out
}

// remaining 返回流结束时缓冲区中未以空行结尾的残余事件载荷（尽力提取，规范外容错）。
func (s *sseScanner) remaining() []string {
	rest := s.buf
	s.buf = nil
	if len(bytes.TrimSpace(rest)) == 0 {
		return nil
	}
	if payload := sseDataPayload(rest); payload != "" {
		return []string{payload}
	}
	return nil
}

// sseEventEnd 查找事件结束分隔符位置（\n\n 或 \r\n\r\n），返回 (索引, 分隔长度)；未找到返回 (-1,0)。
// v1.8.4（M4）：按“位置先后”而非“分隔符类型优先级”切割——两种分隔符都查找、取更早
// 出现者。旧实现先全局找 \n\n 再找 \r\n\r\n，混合行尾流（CRLF 分隔事件在前、LF 分隔事件
// 在后，常见于多层代理/网关转写）中 \n\n 位置更靠后却被优先选中，导致前面所有 CRLF 分隔
// 事件与后续事件合并为一个事件块，多行 data 拼接后成为非法 JSON、成批丢弃。
// 注：\r\n\r\n（0d 0a 0d 0a）内部不含连续 0a 0a，两种分隔符位置不会重叠，取小者安全。
func sseEventEnd(b []byte) (int, int) {
	i1 := bytes.Index(b, []byte("\n\n"))
	i2 := bytes.Index(b, []byte("\r\n\r\n"))
	if i1 >= 0 && i2 >= 0 {
		if i1 < i2 {
			return i1, 2
		}
		return i2, 4
	}
	if i1 >= 0 {
		return i1, 2
	}
	if i2 >= 0 {
		return i2, 4
	}
	return -1, 0
}

// sseDataPayload 从单个事件块中提取 data 载荷（多行 data: 以 \n 拼接）。
func sseDataPayload(event []byte) string {
	var lines []string
	for _, line := range bytes.Split(event, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		lines = append(lines, string(bytes.TrimPrefix(trimmed, []byte("data:"))))
	}
	if len(lines) == 0 {
		return ""
	}
	// 按规范去掉 data: 后的单个前导空格
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, " ")
	}
	return strings.Join(lines, "\n")
}

// ===== 上游解析器（payload → []StreamEvent）=====

// ParseStreamPayload 将上游一个 SSE data 载荷解析为归一化事件序列。
// 无法识别/无内容的载荷返回空切片（不报错，尽力转发）。
func ParseStreamPayload(apiType string, payload string) []StreamEvent {
	switch NormalizeAPIType(apiType) {
	case FormatResponses:
		return parseResponsesStreamPayload(payload)
	case FormatAnthropic:
		return parseAnthropicStreamPayload(payload)
	default:
		return parseOpenAIStreamPayload(payload)
	}
}

// parseOpenAIStreamPayload 解析 OpenAI Chat Completions 的流式 chunk。
func parseOpenAIStreamPayload(payload string) []StreamEvent {
	var chunk struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Role      string  `json:"role"`
				Content   *string `json:"content"`
				Reasoning string  `json:"reasoning_content"` // v1.8.4（M6）：DeepSeek R1/兼容网关惯例
				Thinking  string  `json:"reasoning"`         // v1.8.4（M6）：部分网关的短字段名
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function *struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return nil
	}
	var evs []StreamEvent
	if chunk.Error != nil {
		return append(evs, StreamEvent{Kind: evError, Text: chunk.Error.Message})
	}
	if chunk.ID != "" || chunk.Model != "" {
		evs = append(evs, StreamEvent{Kind: evStart, ID: chunk.ID, Model: chunk.Model})
	}
	if chunk.Usage != nil && (chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0) {
		evs = append(evs, StreamEvent{Kind: evUsage,
			Prompt: chunk.Usage.PromptTokens, Completion: chunk.Usage.CompletionTokens, Total: chunk.Usage.TotalTokens})
	}
	if len(chunk.Choices) > 0 {
		ch := chunk.Choices[0]
		// v1.8.4（M6）：思考增量优先于正文（reasoning_content 为 DeepSeek/兼容网关惯例字段名）
		if r := nonEmpty(ch.Delta.Reasoning, ch.Delta.Thinking); r != "" {
			evs = append(evs, StreamEvent{Kind: evThinking, Text: r})
		}
		for _, tc := range ch.Delta.ToolCalls {
			if tc.ID != "" || (tc.Function != nil && tc.Function.Name != "") {
				name := ""
				if tc.Function != nil {
					name = tc.Function.Name
				}
				evs = append(evs, StreamEvent{Kind: evToolStart, ToolIndex: tc.Index, ToolID: tc.ID, ToolName: name})
			}
			if tc.Function != nil && tc.Function.Arguments != "" {
				evs = append(evs, StreamEvent{Kind: evToolArgs, ToolIndex: tc.Index, ToolArgs: tc.Function.Arguments})
			}
		}
		if ch.Delta.Content != nil && *ch.Delta.Content != "" {
			evs = append(evs, StreamEvent{Kind: evText, Text: *ch.Delta.Content})
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			evs = append(evs, StreamEvent{Kind: evFinish, Finish: *ch.FinishReason})
		}
	}
	return evs
}

// parseAnthropicStreamPayload 解析 Anthropic Messages 的流式事件。
func parseAnthropicStreamPayload(payload string) []StreamEvent {
	var ev struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
			Thinking    string `json:"thinking"` // v1.8.4（M6）：thinking_delta 的思考增量
		} `json:"delta"`
		ContentBlock *struct {
			Type     string `json:"type"`
			ID       string `json:"id"`
			Name     string `json:"name"`
			Thinking string `json:"thinking"` // v1.8.4（M6）：thinking 块的思考内容（start 帧可携带）
		} `json:"content_block"`
		Message *struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return nil
	}
	var evs []StreamEvent
	switch ev.Type {
	case "message_start":
		e := StreamEvent{Kind: evStart, ID: "msg_llmproxy", Model: "claude"}
		if ev.Message != nil {
			if ev.Message.ID != "" {
				e.ID = ev.Message.ID
			}
			if ev.Message.Model != "" {
				e.Model = ev.Message.Model
			}
			if ev.Message.Usage != nil && ev.Message.Usage.InputTokens > 0 {
				evs = append(evs, StreamEvent{Kind: evUsage, Prompt: ev.Message.Usage.InputTokens})
			}
		}
		evs = append(evs, e)
	case "content_block_start":
		// v1.8.4（M6）：extended thinking 块的 start 事件；signature 不可跨格式重建（跳过，
		// 客户端回传历史时我们的解析器同样忽略 thinking 块，不会携带无效 signature 回上游）
		if ev.ContentBlock != nil && ev.ContentBlock.Type == "thinking" && ev.ContentBlock.Thinking != "" {
			evs = append(evs, StreamEvent{Kind: evThinking, Text: ev.ContentBlock.Thinking})
		}
		if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
			evs = append(evs, StreamEvent{Kind: evToolStart,
				ToolIndex: ev.Index, ToolID: ev.ContentBlock.ID, ToolName: ev.ContentBlock.Name})
		}
	case "content_block_delta":
		switch ev.Delta.Type {
		case "thinking_delta": // v1.8.4（M6）：思考增量
			if ev.Delta.Thinking != "" {
				evs = append(evs, StreamEvent{Kind: evThinking, Text: ev.Delta.Thinking})
			}
		case "text_delta":
			if ev.Delta.Text != "" {
				evs = append(evs, StreamEvent{Kind: evText, Text: ev.Delta.Text})
			}
		case "input_json_delta":
			if ev.Delta.PartialJSON != "" {
				evs = append(evs, StreamEvent{Kind: evToolArgs, ToolIndex: ev.Index, ToolArgs: ev.Delta.PartialJSON})
			}
		}
	case "content_block_stop":
		evs = append(evs, StreamEvent{Kind: evToolEnd, ToolIndex: ev.Index})
	case "message_delta":
		if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
			evs = append(evs, StreamEvent{Kind: evUsage, Completion: ev.Usage.OutputTokens})
		}
		if ev.Delta.StopReason != "" {
			evs = append(evs, StreamEvent{Kind: evFinish, Finish: anthropicToFinish(ev.Delta.StopReason)})
		}
	case "message_stop":
		evs = append(evs, StreamEvent{Kind: evDone})
	case "error":
		msg := "upstream error"
		if ev.Error != nil && ev.Error.Message != "" {
			msg = ev.Error.Message
		}
		evs = append(evs, StreamEvent{Kind: evError, Text: msg})
	}
	return evs
}

// parseResponsesStreamPayload 解析 OpenAI Responses API 的流式事件。
func parseResponsesStreamPayload(payload string) []StreamEvent {
	var ev struct {
		Type        string `json:"type"`
		OutputIndex int    `json:"output_index"`
		Item        *struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"item"`
		Delta     string `json:"delta"`
		Arguments string `json:"arguments"`
		Message   string `json:"message"` // 顶层错误消息（官方 type=error 事件）
		Response  *struct {
			ID     string `json:"id"`
			Model  string `json:"model"`
			Status string `json:"status"`
			Usage  *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
				TotalTokens  int64 `json:"total_tokens"`
			} `json:"usage"`
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"` // 官方 response.failed 的错误在 response.error 内
		} `json:"response"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return nil
	}
	var evs []StreamEvent
	switch ev.Type {
	case "response.created", "response.in_progress":
		e := StreamEvent{Kind: evStart, ID: "resp_llmproxy", Model: "gpt"}
		if ev.Response != nil {
			if ev.Response.ID != "" {
				e.ID = ev.Response.ID
			}
			if ev.Response.Model != "" {
				e.Model = ev.Response.Model
			}
		}
		evs = append(evs, e)
	case "response.output_item.added":
		if ev.Item != nil && ev.Item.Type == "function_call" {
			evs = append(evs, StreamEvent{Kind: evToolStart,
				ToolIndex: ev.OutputIndex, ToolID: ev.Item.CallID, ToolName: ev.Item.Name})
		}
	case "response.output_text.delta":
		if ev.Delta != "" {
			evs = append(evs, StreamEvent{Kind: evText, Text: ev.Delta})
		}
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		// v1.8.4（M6）：Responses 推理条目的思考增量（官方两种事件形态均归一）
		if ev.Delta != "" {
			evs = append(evs, StreamEvent{Kind: evThinking, Text: ev.Delta})
		}
	case "response.function_call_arguments.delta":
		if ev.Delta != "" {
			evs = append(evs, StreamEvent{Kind: evToolArgs, ToolIndex: ev.OutputIndex, ToolArgs: ev.Delta})
		}
	case "response.output_item.done":
		if ev.Item != nil && ev.Item.Type == "function_call" {
			evs = append(evs, StreamEvent{Kind: evToolEnd, ToolIndex: ev.OutputIndex})
		}
	case "response.completed", "response.incomplete":
		finish := "stop"
		if ev.Response != nil && ev.Response.IncompleteDetails != nil &&
			ev.Response.IncompleteDetails.Reason == "max_output_tokens" {
			finish = "length"
		}
		if ev.Response != nil && ev.Response.Usage != nil &&
			(ev.Response.Usage.InputTokens > 0 || ev.Response.Usage.OutputTokens > 0) {
			evs = append(evs, StreamEvent{Kind: evUsage,
				Prompt: ev.Response.Usage.InputTokens, Completion: ev.Response.Usage.OutputTokens,
				Total: ev.Response.Usage.TotalTokens})
		}
		evs = append(evs, StreamEvent{Kind: evFinish, Finish: finish})
		evs = append(evs, StreamEvent{Kind: evDone})
	case "response.failed":
		// 官方语义：错误详情在 response.error；兼容顶层 error 与 response.status 兜底
		msg := "upstream response failed"
		if ev.Response != nil && ev.Response.Error != nil && ev.Response.Error.Message != "" {
			msg = ev.Response.Error.Message
		} else if ev.Error != nil && ev.Error.Message != "" {
			msg = ev.Error.Message
		} else if ev.Response != nil && ev.Response.Status == "failed" {
			msg = "upstream response status is failed"
		}
		evs = append(evs, StreamEvent{Kind: evError, Text: msg})
	case "error":
		// M8 修复：官方流式中途错误事件（event: error / {"type":"error",...}）此前无处理分支，
		// 事件被静默丢弃并在 EOF 时被 finalize 伪造为成功终止流；现归一为 evError
		// （代理层据此按失败处理：未写客户端时可重试，已写则断流记日志）。
		// 错误消息优先顶层 message，兼容嵌套 error.message。
		msg := "upstream error"
		if ev.Message != "" {
			msg = ev.Message
		} else if ev.Error != nil && ev.Error.Message != "" {
			msg = ev.Error.Message
		}
		evs = append(evs, StreamEvent{Kind: evError, Text: msg})
	}
	return evs
}

// ===== 客户端发射器（StreamEvent → 客户端 SSE 帧）=====

// StreamEmitter 消费归一化事件并输出目标客户端格式的 SSE 帧。
type StreamEmitter interface {
	// Feed 处理一个事件，返回需要立即写出的帧（可为空）。
	// 实现内部保证：首个内容事件前自动输出开场帧（message_start / response.created / role chunk）。
	Feed(ev StreamEvent) []byte
	// Finish 输出收尾帧（幂等；finish 为 IR finish_reason，usage 为最终用量）。
	Finish(finish string, prompt, completion, total int64) []byte
}

// NewStreamEmitter 按客户端格式创建发射器。
func NewStreamEmitter(format, model, id string) StreamEmitter {
	switch NormalizeAPIType(format) {
	case FormatAnthropic:
		return &anthropicEmitter{model: model, id: id}
	case FormatResponses:
		return &responsesEmitter{model: model, id: id}
	default:
		return &openAIEmitter{model: model, id: id}
	}
}

// ----- OpenAI Chat Completions 发射器 -----

type openAIEmitter struct {
	model, id string
	roleSent  bool
	finished  bool
	created   int64
}

func (e *openAIEmitter) chunkBytes(delta map[string]interface{}, finish interface{}, withUsage bool, p, c, t int64) []byte {
	var buf bytes.Buffer
	id := nonEmpty(e.id, "chatcmpl-llmproxy")
	created := e.created
	if created == 0 {
		// v1.8.4（C5）：首个输出帧固化 created，后续帧复用——官方语义为同一响应
		// 所有 chunk 时间戳恒定，旧实现逐帧取当前时间导致跨秒流内不一致。
		e.created = nowUnix()
		created = e.created
	}
	choice := map[string]interface{}{"index": 0, "delta": delta, "finish_reason": finish}
	m := map[string]interface{}{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": nonEmpty(e.model, "gpt"),
		"choices": []map[string]interface{}{choice},
	}
	if withUsage {
		m["usage"] = map[string]int64{"prompt_tokens": p, "completion_tokens": c, "total_tokens": t}
	}
	sseFrame(&buf, "", m)
	return buf.Bytes()
}

func (e *openAIEmitter) Feed(ev StreamEvent) []byte {
	var buf bytes.Buffer
	if !e.roleSent {
		e.roleSent = true
		buf.Write(e.chunkBytes(map[string]interface{}{"role": "assistant", "content": ""}, nil, false, 0, 0, 0))
	}
	switch ev.Kind {
	case evText:
		buf.Write(e.chunkBytes(map[string]interface{}{"content": ev.Text}, nil, false, 0, 0, 0))
	case evThinking:
		// v1.8.4（M6）：思考增量走 reasoning_content（DeepSeek R1/兼容网关惯例字段，
		// 官方上游会忽略未知字段，客户端 SDK 可识别）
		buf.Write(e.chunkBytes(map[string]interface{}{"reasoning_content": ev.Text}, nil, false, 0, 0, 0))
	case evToolStart:
		buf.Write(e.chunkBytes(map[string]interface{}{
			"tool_calls": []map[string]interface{}{{
				"index": ev.ToolIndex, "id": ev.ToolID, "type": "function",
				"function": map[string]string{"name": ev.ToolName, "arguments": ""},
			}},
		}, nil, false, 0, 0, 0))
	case evToolArgs:
		buf.Write(e.chunkBytes(map[string]interface{}{
			"tool_calls": []map[string]interface{}{{
				"index": ev.ToolIndex, "type": "function",
				"function": map[string]string{"arguments": ev.ToolArgs},
			}},
		}, nil, false, 0, 0, 0))
	}
	return buf.Bytes()
}

func (e *openAIEmitter) Finish(finish string, p, c, t int64) []byte {
	if e.finished {
		return nil
	}
	e.finished = true
	var buf bytes.Buffer
	if !e.roleSent {
		e.roleSent = true
		buf.Write(e.chunkBytes(map[string]interface{}{"role": "assistant", "content": ""}, nil, false, 0, 0, 0))
	}
	buf.Write(e.chunkBytes(map[string]interface{}{}, nonEmpty(finish, "stop"), true, p, c, t))
	buf.WriteString("data: [DONE]\n\n")
	return buf.Bytes()
}

// ----- Anthropic Messages 发射器 -----

type anthropicEmitter struct {
	model, id    string
	started      bool // message_start 已发
	finished     bool
	textOpen     bool // 文本块打开中
	textBlockIdx int  // 当前文本块的 index（每次打开分配新索引）
	thinkingOpen bool // v1.8.4（M6）：thinking 块打开中
	thinkingIdx  int  // 当前 thinking 块的 index
	nBlocks      int  // 下一个可分配的内容块号（文本块与工具块统一从此分配，单调递增）
	toolBlocks   map[int]int
	toolArgsSeen map[int]bool // 客户端块索引 → 是否已发送过参数增量（M5：无参工具补 "{}"）
	closedTool   map[int]bool
}

// openThinkingBlock 打开 thinking 块（v1.8.4 M6）：思考增量到达时惰性开启；
// signature 无法跨格式重建（留空），客户端展示用途不校验；客户端回传历史时
// 我们的请求解析器忽略 thinking 块，不会携带无效 signature 回上游。
func (e *anthropicEmitter) openThinkingBlock() []byte {
	if e.thinkingOpen {
		return nil
	}
	e.thinkingOpen = true
	e.thinkingIdx = e.nBlocks
	e.nBlocks++
	var buf bytes.Buffer
	sseFrame(&buf, "content_block_start", map[string]interface{}{
		"type": "content_block_start", "index": e.thinkingIdx,
		"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
	})
	return buf.Bytes()
}

// closeThinkingBlock 关闭 thinking 块（首个正文/工具事件或收尾时）。
func (e *anthropicEmitter) closeThinkingBlock() []byte {
	if !e.thinkingOpen {
		return nil
	}
	e.thinkingOpen = false
	var buf bytes.Buffer
	sseFrame(&buf, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": e.thinkingIdx})
	return buf.Bytes()
}

func (e *anthropicEmitter) ensureStart() []byte {
	if e.started {
		return nil
	}
	e.started = true
	var buf bytes.Buffer
	sseFrame(&buf, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": nonEmpty(e.id, "msg_llmproxy"), "type": "message", "role": "assistant",
			"model":   nonEmpty(e.model, "claude"),
			"content": []interface{}{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]int64{"input_tokens": 0, "output_tokens": 0},
		},
	})
	return buf.Bytes()
}

// openTextBlock 打开文本块。
// M7 修复：文本块与工具块统一从 nBlocks 顺序分配索引，不再硬编码 index 0——
// 上游先工具后文本（或文本→工具→文本交替）时，旧实现会使 text 块与 tool 块
// 重复使用 index 0，产出非法 Anthropic 事件序列（严格 SDK 解析失败断连）。
func (e *anthropicEmitter) openTextBlock() []byte {
	if e.textOpen {
		return nil
	}
	e.textOpen = true
	e.textBlockIdx = e.nBlocks
	e.nBlocks++
	var buf bytes.Buffer
	sseFrame(&buf, "content_block_start", map[string]interface{}{
		"type": "content_block_start", "index": e.textBlockIdx,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	})
	return buf.Bytes()
}

func (e *anthropicEmitter) closeTextBlock() []byte {
	if !e.textOpen {
		return nil
	}
	e.textOpen = false
	var buf bytes.Buffer
	sseFrame(&buf, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": e.textBlockIdx})
	return buf.Bytes()
}

func (e *anthropicEmitter) Feed(ev StreamEvent) []byte {
	var buf bytes.Buffer
	switch ev.Kind {
	case evStart:
		if ev.ID != "" {
			e.id = ev.ID
		}
		if ev.Model != "" {
			e.model = ev.Model
		}
		buf.Write(e.ensureStart())
	case evText:
		buf.Write(e.ensureStart())
		buf.Write(e.closeThinkingBlock()) // v1.8.4（M6）：思考块先于正文块关闭
		buf.Write(e.openTextBlock())
		sseFrame(&buf, "content_block_delta", map[string]interface{}{
			"type": "content_block_delta", "index": e.textBlockIdx,
			"delta": map[string]interface{}{"type": "text_delta", "text": ev.Text},
		})
	case evThinking:
		// v1.8.4（M6）：思考增量
		buf.Write(e.ensureStart())
		buf.Write(e.openThinkingBlock())
		sseFrame(&buf, "content_block_delta", map[string]interface{}{
			"type": "content_block_delta", "index": e.thinkingIdx,
			"delta": map[string]interface{}{"type": "thinking_delta", "thinking": ev.Text},
		})
	case evToolStart:
		buf.Write(e.ensureStart())
		buf.Write(e.closeThinkingBlock()) // v1.8.4（M6）
		buf.Write(e.closeTextBlock())
		if e.toolBlocks == nil {
			e.toolBlocks = map[int]int{}
			e.toolArgsSeen = map[int]bool{}
			e.closedTool = map[int]bool{}
		}
		idx := e.nBlocks
		e.nBlocks = idx + 1
		e.toolBlocks[ev.ToolIndex] = idx
		sseFrame(&buf, "content_block_start", map[string]interface{}{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]interface{}{
				"type": "tool_use", "id": nonEmpty(ev.ToolID, "toolu_llmproxy"), "name": ev.ToolName,
				"input": map[string]interface{}{},
			},
		})
	case evToolArgs:
		if idx, ok := e.toolBlocks[ev.ToolIndex]; ok {
			e.toolArgsSeen[idx] = true
			sseFrame(&buf, "content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": idx,
				"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": ev.ToolArgs},
			})
		}
	case evToolEnd:
		if idx, ok := e.toolBlocks[ev.ToolIndex]; ok && !e.closedTool[idx] {
			e.closedTool[idx] = true
			if !e.toolArgsSeen[idx] {
				// v1.8.4（M5）：无参工具全程无参数增量——上游只发过 name+空 arguments 首片。
				// 补发一次 partial_json "{}"，否则客户端聚合 input 为空串、JSON 解析失败
				// （官方流对 input:{} 的 tool_use 至少发一次 "{}"）。
				sseFrame(&buf, "content_block_delta", map[string]interface{}{
					"type": "content_block_delta", "index": idx,
					"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": "{}"},
				})
			}
			sseFrame(&buf, "content_block_stop", map[string]interface{}{
				"type": "content_block_stop", "index": idx,
			})
		}
	}
	return buf.Bytes()
}

func (e *anthropicEmitter) Finish(finish string, p, c, t int64) []byte {
	if e.finished {
		return nil
	}
	e.finished = true
	var buf bytes.Buffer
	buf.Write(e.ensureStart())
	buf.Write(e.closeThinkingBlock()) // v1.8.4（M6）：未关的 thinking 块收尾
	buf.Write(e.closeTextBlock())
	sseFrame(&buf, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": finishToAnthropic(nonEmpty(finish, "stop")), "stop_sequence": nil},
		"usage": map[string]int64{"output_tokens": c},
	})
	sseFrame(&buf, "message_stop", map[string]interface{}{"type": "message_stop"})
	return buf.Bytes()
}

// ----- OpenAI Responses 发射器 -----

type responsesEmitter struct {
	model, id string
	started   bool
	finished  bool
	createdAt int64 // v1.8.4（C5）：首个输出帧固化，后续事件复用（旧实现逐帧取当前时间）

	reasoningOpen bool // v1.8.4（M6）：reasoning 条目打开中
	reasoningIdx  int
	reasonAccum   strings.Builder // 当前 reasoning 条目的思考累积
	textOpen      bool
	textAccum     strings.Builder          // 当前打开文本块的累积（块级：output_text.done 用，关闭时重置，v1.8.2 修复 V-P3）
	totalText     strings.Builder          // 全响应输出文本（跨块累计：response.completed 的 output_text 用）
	textIdx       int                      // 文本条目的 output_index（首个条目时为 0）
	nextIdx       int                      // 下一个可分配的 output_index
	items         []map[string]interface{} // 已完成的 output 条目（空流时输出 [] 而非 null）
	toolIdx       map[int]int              // 顺序工具索引 → output_index
	toolMeta      map[int]*respToolMeta
}

type respToolMeta struct {
	itemID  string
	callID  string
	name    string
	argsAcc strings.Builder
}

func (e *responsesEmitter) ensureStart() []byte {
	if e.started {
		return nil
	}
	e.started = true
	if e.items == nil {
		e.items = []map[string]interface{}{}
	}
	if e.createdAt == 0 {
		e.createdAt = nowUnix() // v1.8.4（C5）：created_at 全响应恒定
	}
	var buf bytes.Buffer
	resp := func(status string) map[string]interface{} {
		return map[string]interface{}{
			"id": nonEmpty(e.id, "resp_llmproxy"), "object": "response",
			"created_at": e.createdAt, "status": status, "model": nonEmpty(e.model, "gpt"),
			"output": []interface{}{},
		}
	}
	sseFrame(&buf, "response.created", map[string]interface{}{"type": "response.created", "response": resp("in_progress")})
	sseFrame(&buf, "response.in_progress", map[string]interface{}{"type": "response.in_progress", "response": resp("in_progress")})
	return buf.Bytes()
}

func (e *responsesEmitter) openTextItem() []byte {
	if e.textOpen {
		return nil
	}
	e.textOpen = true
	e.textIdx = e.nextIdx
	e.nextIdx++
	var buf bytes.Buffer
	sseFrame(&buf, "response.output_item.added", map[string]interface{}{
		"type": "response.output_item.added", "output_index": e.textIdx,
		"item": map[string]interface{}{
			"type": "message", "id": "msg_llmproxy", "status": "in_progress", "role": "assistant",
			"content": []interface{}{},
		},
	})
	sseFrame(&buf, "response.content_part.added", map[string]interface{}{
		"type": "response.content_part.added", "item_id": "msg_llmproxy",
		"output_index": e.textIdx, "content_index": 0,
		"part": map[string]interface{}{"type": "output_text", "text": "", "annotations": []interface{}{}},
	})
	return buf.Bytes()
}

// openReasoningItem 打开 reasoning 条目（v1.8.4 M6）：思考增量到达时惰性开启。
// 事件结构对齐官方 reasoning 条目流：output_item.added → reasoning_text.delta… → 收尾关闭。
func (e *responsesEmitter) openReasoningItem() []byte {
	if e.reasoningOpen {
		return nil
	}
	e.reasoningOpen = true
	e.reasoningIdx = e.nextIdx
	e.nextIdx++
	e.reasonAccum = strings.Builder{}
	var buf bytes.Buffer
	sseFrame(&buf, "response.output_item.added", map[string]interface{}{
		"type": "response.output_item.added", "output_index": e.reasoningIdx,
		"item": map[string]interface{}{
			"type": "reasoning", "id": "rs_llmproxy", "summary": []interface{}{}, "status": "in_progress",
		},
	})
	return buf.Bytes()
}

// closeReasoningItem 关闭 reasoning 条目（首个正文/工具事件或收尾时）。
func (e *responsesEmitter) closeReasoningItem() []byte {
	if !e.reasoningOpen {
		return nil
	}
	e.reasoningOpen = false
	text := e.reasonAccum.String()
	e.reasonAccum = strings.Builder{}
	var buf bytes.Buffer
	sseFrame(&buf, "response.output_item.done", map[string]interface{}{
		"type": "response.output_item.done", "output_index": e.reasoningIdx,
		"item": map[string]interface{}{
			"type": "reasoning", "id": "rs_llmproxy", "summary": []interface{}{},
			"content": []map[string]interface{}{{"type": "reasoning_text", "text": text}},
			"status":  "completed",
		},
	})
	if e.items == nil {
		e.items = []map[string]interface{}{}
	}
	e.items = append(e.items, map[string]interface{}{
		"type": "reasoning", "id": "rs_llmproxy", "summary": []interface{}{},
		"content": []map[string]interface{}{{"type": "reasoning_text", "text": text}},
		"status":  "completed",
	})
	return buf.Bytes()
}

// closeTextItem 关闭当前文本条目并发出 done 事件。
// v1.8.2（V-P3）：done/completed 的块级文本取当前块的累积后重置 textAccum——
// 文本→工具→文本交替流中，后续文本块的 done 不再携带前序块的累积内容；
// 全响应口径的 completed.output_text 改由 totalText 提供。
func (e *responsesEmitter) closeTextItem() []byte {
	if !e.textOpen {
		return nil
	}
	e.textOpen = false
	text := e.textAccum.String()
	e.textAccum = strings.Builder{} // 块级累积重置，下一块从零开始
	var buf bytes.Buffer
	sseFrame(&buf, "response.output_text.done", map[string]interface{}{
		"type": "response.output_text.done", "item_id": "msg_llmproxy",
		"output_index": e.textIdx, "content_index": 0, "text": text,
	})
	sseFrame(&buf, "response.content_part.done", map[string]interface{}{
		"type": "response.content_part.done", "item_id": "msg_llmproxy",
		"output_index": e.textIdx, "content_index": 0,
		"part": map[string]interface{}{"type": "output_text", "text": text, "annotations": []interface{}{}},
	})
	e.items = append(e.items, map[string]interface{}{
		"type": "message", "id": "msg_llmproxy", "status": "completed", "role": "assistant",
		"content": []map[string]interface{}{{"type": "output_text", "text": text, "annotations": []interface{}{}}},
	})
	sseFrame(&buf, "response.output_item.done", map[string]interface{}{
		"type": "response.output_item.done", "output_index": e.textIdx,
		"item": e.items[len(e.items)-1],
	})
	return buf.Bytes()
}

func (e *responsesEmitter) Feed(ev StreamEvent) []byte {
	var buf bytes.Buffer
	switch ev.Kind {
	case evStart:
		if ev.ID != "" {
			e.id = ev.ID
		}
		if ev.Model != "" {
			e.model = ev.Model
		}
		buf.Write(e.ensureStart())
	case evText:
		buf.Write(e.ensureStart())
		buf.Write(e.closeReasoningItem()) // v1.8.4（M6）：reasoning 条目先于正文条目关闭
		buf.Write(e.openTextItem())
		e.textAccum.WriteString(ev.Text)
		e.totalText.WriteString(ev.Text) // 全响应口径：completed.output_text 需跨块累计
		sseFrame(&buf, "response.output_text.delta", map[string]interface{}{
			"type": "response.output_text.delta", "item_id": "msg_llmproxy",
			"output_index": e.textIdx, "content_index": 0, "delta": ev.Text,
		})
	case evThinking:
		// v1.8.4（M6）：思考增量
		buf.Write(e.ensureStart())
		buf.Write(e.openReasoningItem())
		e.reasonAccum.WriteString(ev.Text)
		sseFrame(&buf, "response.reasoning_text.delta", map[string]interface{}{
			"type": "response.reasoning_text.delta", "item_id": "rs_llmproxy",
			"output_index": e.reasoningIdx, "content_index": 0, "delta": ev.Text,
		})
	case evToolStart:
		buf.Write(e.ensureStart())
		buf.Write(e.closeReasoningItem()) // v1.8.4（M6）
		buf.Write(e.closeTextItem())
		if e.toolIdx == nil {
			e.toolIdx = map[int]int{}
			e.toolMeta = map[int]*respToolMeta{}
		}
		idx := e.nextIdx
		e.nextIdx++
		e.toolIdx[ev.ToolIndex] = idx
		meta := &respToolMeta{itemID: fmt.Sprintf("fc_%d", idx), callID: ev.ToolID, name: ev.ToolName}
		e.toolMeta[ev.ToolIndex] = meta
		sseFrame(&buf, "response.output_item.added", map[string]interface{}{
			"type": "response.output_item.added", "output_index": idx,
			"item": map[string]interface{}{
				"type": "function_call", "id": meta.itemID, "call_id": ev.ToolID,
				"name": ev.ToolName, "arguments": "", "status": "in_progress",
			},
		})
	case evToolArgs:
		if meta := e.toolMeta[ev.ToolIndex]; meta != nil {
			meta.argsAcc.WriteString(ev.ToolArgs)
			sseFrame(&buf, "response.function_call_arguments.delta", map[string]interface{}{
				"type": "response.function_call_arguments.delta", "item_id": meta.itemID,
				"output_index": e.toolIdx[ev.ToolIndex], "delta": ev.ToolArgs,
			})
		}
	case evToolEnd:
		if meta := e.toolMeta[ev.ToolIndex]; meta != nil {
			args := meta.argsAcc.String()
			if args == "" {
				// v1.8.4（M5）：无参工具参数为空串时以 "{}" 输出——空串 arguments 会让
				// Codex 等严格客户端的 serde 解析失败，工具调用整体报废。
				args = "{}"
			}
			done := map[string]interface{}{
				"type": "function_call", "id": meta.itemID, "call_id": meta.callID,
				"name": meta.name, "arguments": args, "status": "completed",
			}
			e.items = append(e.items, done)
			sseFrame(&buf, "response.function_call_arguments.done", map[string]interface{}{
				"type": "response.function_call_arguments.done", "item_id": meta.itemID,
				"output_index": e.toolIdx[ev.ToolIndex], "arguments": args,
			})
			sseFrame(&buf, "response.output_item.done", map[string]interface{}{
				"type": "response.output_item.done", "output_index": e.toolIdx[ev.ToolIndex], "item": done,
			})
		}
	}
	return buf.Bytes()
}

func (e *responsesEmitter) Finish(finish string, p, c, t int64) []byte {
	if e.finished {
		return nil
	}
	e.finished = true
	var buf bytes.Buffer
	buf.Write(e.ensureStart())
	buf.Write(e.closeReasoningItem()) // v1.8.4（M6）：未关的 reasoning 条目收尾
	buf.Write(e.closeTextItem())
	text := e.totalText.String() // 全响应输出文本（跨块累计，v1.8.2 修复 V-P3）
	finalEvent := "response.completed"
	status := "completed"
	var incomplete map[string]interface{}
	if nonEmpty(finish, "stop") == "length" {
		finalEvent = "response.incomplete"
		status = "incomplete"
		incomplete = map[string]interface{}{"reason": "max_output_tokens"}
	}
	finalResp := map[string]interface{}{
		"id": nonEmpty(e.id, "resp_llmproxy"), "object": "response",
		// v1.8.4（C5）：终态事件的 created_at 复用首个输出帧固化的值，与开场事件一致。
		"created_at": e.createdAt, "status": status, "model": nonEmpty(e.model, "gpt"), "output": e.items,
		"usage":       map[string]int64{"input_tokens": p, "output_tokens": c, "total_tokens": t},
		"output_text": text,
	}
	if incomplete != nil {
		finalResp["incomplete_details"] = incomplete
	}
	sseFrame(&buf, finalEvent, map[string]interface{}{"type": finalEvent, "response": finalResp})
	return buf.Bytes()
}

// ===== 组合转换器 =====

// StreamConverter 跨格式流式转换器：吃上游原始字节块，吐目标客户端格式的 SSE 字节块。
type StreamConverter struct {
	from, to, model string
	scanner         sseScanner
	emitter         StreamEmitter // 首个事件到达后按捕获的 ID/模型创建
	emitterID       string
	emitterModel    string

	toolMap     map[int]int  // 上游工具索引 → 客户端工具索引
	toolEnded   map[int]bool // 上游工具索引 → 是否已发 evToolEnd（收尾补发用）
	nTools      int
	usageP      int64
	usageC      int64
	usageT      int64
	sawUsage    bool
	sawStart    bool
	sawText     bool // 已向发射器转发首个文本事件（用于识别"生成阶段开始"）
	sawAnyEvent bool // 流中是否出现过任何可识别事件（v1.8.2 V-P2：全坏帧判定用）
	finishSent  bool
	finalFinish string
}

// NewStreamConverter 创建转换器；from/to 为上游/客户端格式（api_type），model 为真实模型名。
func NewStreamConverter(from, to, model string) *StreamConverter {
	return &StreamConverter{from: from, to: to, model: model}
}

// FeedChunk 处理一段上游字节，返回转换后的客户端 SSE 帧（可为空）。
// 上游在流中发送错误事件时返回 *UpstreamStreamError（同时返回已转换的帧）。
func (c *StreamConverter) FeedChunk(chunk []byte) ([]byte, error) {
	var out bytes.Buffer
	for _, payload := range c.scanner.feed(chunk) {
		frames, err := c.convertPayload(payload)
		out.Write(frames)
		if err != nil {
			return out.Bytes(), err
		}
	}
	return out.Bytes(), nil
}

// Finish 上游流正常结束时调用：处理残余帧并输出收尾帧（含 usage）。
// v1.8.4（C6）：残余帧中的错误事件不再被吞——上游在流末尾发出错误事件且该帧
// 恰无尾随空行（EOF 截断、异常断流时常见）时，旧实现静默丢弃错误并伪造成功
// 收尾帧，客户端收到看似正常的成功流且日志层丢失错误信息；现将错误上抛，
// 由代理层按失败处理（已写客户端则断流记日志）。
func (c *StreamConverter) Finish() ([]byte, error) {
	var out bytes.Buffer
	for _, payload := range c.scanner.remaining() {
		frames, err := c.convertPayload(payload)
		out.Write(frames)
		if err != nil {
			return out.Bytes(), err
		}
	}
	out.Write(c.finalize())
	return out.Bytes(), nil
}

// convertPayload 转换单个上游 data 载荷。
func (c *StreamConverter) convertPayload(payload string) ([]byte, error) {
	if payload == "[DONE]" {
		return c.finalize(), nil
	}
	var out bytes.Buffer
	for _, ev := range ParseStreamPayload(c.from, payload) {
		c.sawAnyEvent = true // 出现过至少一个可识别事件（v1.8.2 V-P2：全坏帧流判定）
		switch ev.Kind {
		case evError:
			return out.Bytes(), &UpstreamStreamError{Message: ev.Text}
		case evStart:
			if ev.ID != "" {
				c.emitterID = ev.ID
			}
			if ev.Model != "" {
				c.emitterModel = ev.Model
			}
			if !c.sawStart {
				// 仅首个 start 事件向发射器输出开场帧（后续 chunk 的 id/model 重复出现，跳过）
				c.sawStart = true
				out.Write(c.emit(ev))
			}
		case evUsage:
			if ev.Prompt > 0 {
				c.usageP = ev.Prompt
			}
			if ev.Completion > 0 {
				c.usageC = ev.Completion
			}
			if ev.Total > 0 {
				c.usageT = ev.Total
			}
			c.sawUsage = true
		case evFinish:
			// 只记录 finish 原因，不立即 finalize：OpenAI CC 流的 finish_reason chunk 之后
			// 还会有 usage chunk（stream_options.include_usage）与 [DONE]，立即终止会把
			// 终止帧（含 usage）提前发出——usage 统计归零，且客户端收到终止帧后按正常
			// 流程关闭连接，此时代理还在等上游剩余数据，会把正常完成误判为"客户端断开"。
			// 终止帧统一延迟到明确的流结束信号：[DONE] / Anthropic message_stop /
			// Responses response.completed / 上游 EOF（Finish）。
			c.finalFinish = ev.Finish
		case evDone:
			out.Write(c.finalize())
		case evToolStart:
			if _, ok := c.toolMap[ev.ToolIndex]; !ok {
				if c.toolMap == nil {
					c.toolMap = map[int]int{}
				}
				c.toolMap[ev.ToolIndex] = c.nTools
				c.nTools++
				ev.ToolIndex = c.toolMap[ev.ToolIndex]
				out.Write(c.emit(ev))
			}
			// v1.8.4（C8）：已注册 index 的重复 tool_start（个别兼容网关把 function.name
			// 拆多个 delta 分片，每个非空分片都会再触发一次 start）不再转发——
			// 防止 emitter 重复开块、块索引被覆盖造成事件结构错乱；代价是分片 name
			// 只保留首片（规范上游 name 一次性发送，不受影响）。
			continue
		case evToolArgs:
			if idx, ok := c.toolMap[ev.ToolIndex]; ok {
				ev.ToolIndex = idx
			}
			out.Write(c.emit(ev))
		case evToolEnd:
			// 记录该上游工具条目已结束（收尾时不再补发，见 finalize）
			if c.toolEnded == nil {
				c.toolEnded = map[int]bool{}
			}
			c.toolEnded[ev.ToolIndex] = true
			if idx, ok := c.toolMap[ev.ToolIndex]; ok {
				ev.ToolIndex = idx
			}
			out.Write(c.emit(ev))
		default:
			out.Write(c.emit(ev))
		}
	}
	return out.Bytes(), nil
}

// emit 惰性创建发射器并转发事件。
func (c *StreamConverter) emit(ev StreamEvent) []byte {
	if c.emitter == nil {
		c.emitter = NewStreamEmitter(c.to, c.model, c.emitterID)
	}
	// 标记文本事件已转发：用于速率计算时识别"生成阶段开始"。
	// 只识别真正写入到客户端的"用户可见文本"事件，不包括开场 role 等空事件。
	if ev.Kind == evText && ev.Text != "" {
		c.sawText = true
	}
	return c.emitter.Feed(ev)
}

// finalize 输出收尾帧（幂等；上游未显式给 finish 时默认 stop）。
//
// 收尾前为所有「已开始但未结束」的工具条目补发 evToolEnd：OpenAI Chat Completions
// 上游的流式协议没有工具调用结束信号（工具结束由 finish_reason=tool_calls 隐式标记，
// 永不产生 evToolEnd），若不补发：Responses 客户端将缺失 function_call_arguments.done /
// output_item.done，且终态 response.completed 的 output 数组缺 function_call 条目；
// Anthropic 客户端的 tool_use 块将缺失 content_block_stop。官方客户端（如 Codex）
// 严格校验事件序列，收到此类不完整流会解析失败并断开连接。
func (c *StreamConverter) finalize() []byte {
	if c.finishSent {
		return nil
	}
	c.finishSent = true
	var pre bytes.Buffer
	if len(c.toolMap) > 0 {
		ups := make([]int, 0, len(c.toolMap))
		for up := range c.toolMap {
			ups = append(ups, up)
		}
		sort.Ints(ups)
		for _, up := range ups {
			if !c.toolEnded[up] {
				if c.toolEnded == nil {
					c.toolEnded = map[int]bool{}
				}
				c.toolEnded[up] = true
				pre.Write(c.emit(StreamEvent{Kind: evToolEnd, ToolIndex: c.toolMap[up]}))
			}
		}
	}
	finish := nonEmpty(c.finalFinish, "stop")
	if finish == "" {
		finish = "stop"
	}
	total := c.usageT
	if total == 0 {
		total = c.usageP + c.usageC
	}
	if c.emitter == nil {
		c.emitter = NewStreamEmitter(c.to, c.model, c.emitterID)
	}
	rest := c.emitter.Finish(finish, c.usageP, c.usageC, total)
	return append(pre.Bytes(), rest...)
}

// Finished 返回终止帧（emitter.Finish）是否已发出。
// 用于代理层判定：终止帧已成功写给客户端后，客户端关闭连接属于正常完成，
// 不应记为"客户端断开"失败。
func (c *StreamConverter) Finished() bool {
	return c.finishSent
}

// HasAnyEvent 返回流中是否出现过任何可识别事件（start/文本/工具/usage/finish，v1.8.2）。
// 用于代理层判定「上游返回 200 + SSE 但全部帧无法解析」（如 data: <html> 坏帧）的场景：
// 无任何事件 ⇒ 转换器从未产生输出 ⇒ 客户端未收到任何字节，可安全按失败重试——
// 而不是在 EOF 时伪造成功终止流（V-P2 修复）。
func (c *StreamConverter) HasAnyEvent() bool {
	return c.sawAnyEvent
}

// Usage 返回已累积的用量（prompt, completion, total）；未收到 usage 时返回 false。
func (c *StreamConverter) Usage() (int64, int64, int64, bool) {
	return c.usageP, c.usageC, c.usageT, c.sawUsage
}

// FirstContentEmitted 返回是否已向客户端写出首个非空文本事件。
// 用于上游 → 代理 → 客户端链路在速率计算时识别"生成阶段开始"——
// reasoning 流模型（如 qwen3.8-flash-next）整个响应都在生成，
// 但 reasoning 阶段不会触发此方法（只匹配 evText 且 Text 非空）；
// 当上游切到 content 阶段（首个非空 evText 写出）后，调用方可记录此刻为
// 真正的生成起点，配合末字节时间计算"用户感知到的输出速率"。
func (c *StreamConverter) FirstContentEmitted() bool {
	return c.sawText
}
