package convert

// 回归测试：
//   Anthropic 图文混排 user 消息跨格式转换文本丢失
//   SSE 混合行尾事件按分隔符类型而非位置切割，批量丢事件
//   无参工具跨格式流式参数为空串，客户端 JSON 解析失败
//   发射器 created/created_at 时间戳每帧重取
//   空 text 片段构建时不过滤
//   同 index 重复 tool_start 重复开块
//   thinking/reasoning 跨格式转换（非流式 + 流式 + 伪流式）

import (
	"encoding/json"
	"strings"
	"testing"
)

// ===== Anthropic 图文混排文本丢失 =====

func TestAnthropicTextImageCrossFormat(t *testing.T) {
	body := []byte(`{"model":"claude-3-5-sonnet","max_tokens":1024,"messages":[{"role":"user","content":[
		{"type":"text","text":"这是什么动物"},
		{"type":"image","source":{"type":"url","url":"https://example.com/cat.png"}}]}]}`)

	ir, err := ParseRequest(FormatAnthropic, body)
	if err != nil {
		t.Fatalf("解析 Anthropic 请求失败: %v", err)
	}
	if len(ir.Messages) == 0 {
		t.Fatalf("IR 无消息")
	}

	// 方向一：Anthropic → OpenAI Chat Completions
	up, err := BuildRequest(FormatOpenAI, ir, "gpt-4o")
	if err != nil {
		t.Fatalf("构建 OpenAI 请求失败: %v", err)
	}
	if !strings.Contains(string(up), "这是什么动物") {
		t.Errorf("（OpenAI 方向）：上游请求丢失了用户文本，实际: %s", up)
	}

	// 方向二：Anthropic → Responses
	up2, err := BuildRequest(FormatResponses, ir, "gpt-4o")
	if err != nil {
		t.Fatalf("构建 Responses 请求失败: %v", err)
	}
	if !strings.Contains(string(up2), "这是什么动物") {
		t.Errorf("（Responses 方向）：上游请求丢失了用户文本，实际: %s", up2)
	}
}

// ===== SSE 混合行尾 =====

func TestSSEMixedLineEndings(t *testing.T) {
	// CRLF 分隔的事件在前、LF 分隔的事件在后（中间有多层代理转写时会出现混合行尾）
	s := &sseScanner{}
	payloads := s.feed([]byte("data: {\"a\":1}\r\n\r\ndata: {\"b\":2}\n\n"))
	if len(payloads) != 2 {
		t.Fatalf("混合行尾流应解析出 2 个事件，实际 %d 个: %q", len(payloads), payloads)
	}
	var a, b map[string]interface{}
	if err := json.Unmarshal([]byte(payloads[0]), &a); err != nil {
		t.Fatalf("事件1 JSON 解析失败: %v，payload=%q", err, payloads[0])
	}
	if err := json.Unmarshal([]byte(payloads[1]), &b); err != nil {
		t.Fatalf("事件2 JSON 解析失败: %v，payload=%q", err, payloads[1])
	}
	if v, _ := a["a"].(float64); v != 1 {
		t.Errorf("事件1 内容错误: %v", a)
	}
	if v, _ := b["b"].(float64); v != 2 {
		t.Errorf("事件2 内容错误: %v", b)
	}
}

func TestSSEPureCRLFAndLF(t *testing.T) {
	// 纯 CRLF 流与纯 LF 流均应正确切割
	s := &sseScanner{}
	p := s.feed([]byte("data: {\"a\":1}\r\n\r\ndata: {\"a\":2}\r\n\r\n"))
	if len(p) != 2 {
		t.Fatalf("纯 CRLF 流应解析出 2 个事件，实际 %d", len(p))
	}
	s2 := &sseScanner{}
	p2 := s2.feed([]byte("data: {\"a\":1}\n\ndata: {\"a\":2}\n\n"))
	if len(p2) != 2 {
		t.Fatalf("纯 LF 流应解析出 2 个事件，实际 %d", len(p2))
	}
}

// ===== 无参工具跨格式流式 =====

// zeroArgToolStream OpenAI 上游无参工具的典型流：首片 name+空 arguments，无后续参数增量。
const zeroArgToolStream = "data: " + `{"id":"c1","model":"gpt","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"list_tools","arguments":""}}]}}]}` + "\n\n" +
	"data: " + `{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
	"data: [DONE]\n\n"

func TestZeroArgToolAnthropicClient(t *testing.T) {
	conv := NewStreamConverter(FormatOpenAI, FormatAnthropic, "gpt-4o")
	out, err := conv.FeedChunk([]byte(zeroArgToolStream))
	if err != nil {
		t.Fatalf("feed 失败: %v", err)
	}
	if !strings.Contains(string(out), `"partial_json":"{}"`) {
		t.Errorf("（Anthropic 客户端）：无参工具应补发 partial_json \"{}\"，实际输出: %s", out)
	}
}

func TestZeroArgToolResponsesClient(t *testing.T) {
	conv := NewStreamConverter(FormatOpenAI, FormatResponses, "gpt-4o")
	out, err := conv.FeedChunk([]byte(zeroArgToolStream))
	if err != nil {
		t.Fatalf("feed 失败: %v", err)
	}
	if !strings.Contains(string(out), `"arguments":"{}"`) {
		t.Errorf("（Responses 客户端）：arguments.done 应携带 \"{}\" 而非空串，实际输出: %s", out)
	}
}

func TestZeroArgToolPseudoSSE(t *testing.T) {
	// 伪流式兜底路径（上游忽略 stream 返回完整 JSON）：
	// 真实链路为 ParseResponse(pType) → BuildResponse(client) → BuildSSE(client)，
	// 测试按同一链路构造，验证无参工具最终输出参数为 "{}" 而非空串。
	upResp := []byte(`{"id":"c1","model":"gpt","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_tools","arguments":""}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	for _, client := range []string{FormatAnthropic, FormatResponses} {
		ir, err := ParseResponse(FormatOpenAI, upResp)
		if err != nil {
			t.Fatalf("（伪流式 %s）：解析上游响应失败: %v", client, err)
		}
		if len(ir.ToolCalls) != 1 {
			t.Fatalf("（伪流式 %s）：应解析出 1 个工具调用，实际 %d", client, len(ir.ToolCalls))
		}
		clientResp := BuildResponse(client, ir, ir.Model)
		out, err := BuildSSE(client, clientResp)
		if err != nil {
			t.Fatalf("（伪流式 %s）：BuildSSE 失败: %v", client, err)
		}
		if !strings.Contains(string(out), `"{}"`) {
			t.Errorf("（伪流式 %s）：无参工具参数应为 \"{}\" 而非空串，实际输出: %s", client, out)
		}
	}
}

// ===== 发射器时间戳固化 =====

func TestEmitterCreatedPinned(t *testing.T) {
	e := NewStreamEmitter(FormatOpenAI, "gpt", "id1")
	e.Feed(StreamEvent{Kind: evText, Text: "a"})
	if e.(*openAIEmitter).created == 0 {
		t.Errorf("openAI 发射器首帧后应固化 created，而非每帧重取当前时间")
	}
}

// ===== 空 text 片段构建过滤 =====

func TestEmptyTextPartFiltered(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":[
		{"type":"text","text":""},
		{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`)
	ir, err := ParseRequest(FormatOpenAI, body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	up, err := BuildRequest(FormatOpenAI, ir, "gpt")
	if err != nil {
		t.Fatalf("构建 OpenAI 失败: %v", err)
	}
	if strings.Contains(string(up), `"text":""`) {
		t.Errorf("OpenAI 构建应过滤空 text 片段，实际: %s", up)
	}
	up2, err := BuildRequest(FormatResponses, ir, "gpt")
	if err != nil {
		t.Fatalf("构建 Responses 失败: %v", err)
	}
	if strings.Contains(string(up2), `"text":""`) {
		t.Errorf("Responses 构建应过滤空 text 片段，实际: %s", up2)
	}
}

// ===== 重复 tool_start 防御 =====

func TestDuplicateToolStartIgnored(t *testing.T) {
	// 非规范上游把 function.name 拆多个 delta 分片：每个非空分片都会再触发一次 start
	stream := "data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"list"}}]}}]}` + "\n\n" +
		"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"_tools"}}]}}]}` + "\n\n" +
		"data: " + `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	conv := NewStreamConverter(FormatOpenAI, FormatAnthropic, "gpt-4o")
	out, err := conv.FeedChunk([]byte(stream))
	if err != nil {
		t.Fatalf("feed 失败: %v", err)
	}
	if n := strings.Count(string(out), `"type":"tool_use"`); n != 1 {
		t.Errorf("同 index 重复 tool_start 应只开一个工具块，实际 %d 个，输出: %s", n, out)
	}
}

// ===== thinking/reasoning 跨格式转换 =====

func TestReasoningNonStreamCrossFormat(t *testing.T) {
	// OpenAI 上游（reasoning_content）→ Anthropic / Responses 客户端
	up := []byte(`{"id":"c1","model":"deepseek-r1","choices":[{"message":{"role":"assistant","content":"答案是42","reasoning_content":"让我想想"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	ir, err := ParseResponse(FormatOpenAI, up)
	if err != nil {
		t.Fatalf("解析 OpenAI 响应失败: %v", err)
	}
	if ir.Reasoning != "让我想想" || ir.Text != "答案是42" {
		t.Fatalf("reasoning_content 未解析入 IR，Reasoning=%q Text=%q", ir.Reasoning, ir.Text)
	}
	ant := BuildResponse(FormatAnthropic, ir, ir.Model)
	if !strings.Contains(string(ant), `"type":"thinking"`) || !strings.Contains(string(ant), `"thinking":"让我想想"`) {
		t.Errorf("Anthropic 响应应含 thinking 块，实际: %s", ant)
	}
	resp := BuildResponse(FormatResponses, ir, ir.Model)
	if !strings.Contains(string(resp), `"type":"reasoning"`) || !strings.Contains(string(resp), `"text":"让我想想"`) {
		t.Errorf("Responses 响应应含 reasoning 条目，实际: %s", resp)
	}
}

func TestAnthropicThinkingNonStream(t *testing.T) {
	// Anthropic 上游（thinking 块）→ OpenAI 客户端
	up := []byte(`{"id":"m1","type":"message","role":"assistant","model":"claude-3-7","content":[{"type":"thinking","thinking":"先分析问题","signature":"sig"},{"type":"text","text":"结论"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`)
	ir, err := ParseResponse(FormatAnthropic, up)
	if err != nil {
		t.Fatalf("解析 Anthropic 响应失败: %v", err)
	}
	if ir.Reasoning != "先分析问题" || ir.Text != "结论" {
		t.Fatalf("thinking 块未解析入 IR，Reasoning=%q Text=%q", ir.Reasoning, ir.Text)
	}
	cc := BuildResponse(FormatOpenAI, ir, ir.Model)
	if !strings.Contains(string(cc), `"reasoning_content":"先分析问题"`) {
		t.Errorf("OpenAI 响应应含 reasoning_content，实际: %s", cc)
	}
}

func TestResponsesReasoningNonStream(t *testing.T) {
	// Responses 上游（reasoning 条目）→ OpenAI 客户端
	up := []byte(`{"id":"r1","created_at":1,"status":"completed","model":"o4-mini","output":[{"type":"reasoning","id":"rs1","summary":[],"content":[{"type":"reasoning_text","text":"推理中"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"答案"}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)
	ir, err := ParseResponse(FormatResponses, up)
	if err != nil {
		t.Fatalf("解析 Responses 响应失败: %v", err)
	}
	if ir.Reasoning != "推理中" || ir.Text != "答案" {
		t.Fatalf("reasoning 条目未解析入 IR，Reasoning=%q Text=%q", ir.Reasoning, ir.Text)
	}
	cc := BuildResponse(FormatOpenAI, ir, ir.Model)
	if !strings.Contains(string(cc), `"reasoning_content":"推理中"`) {
		t.Errorf("OpenAI 响应应含 reasoning_content，实际: %s", cc)
	}
}

// reasoningStream OpenAI 上游思考流：reasoning_content 两段 + 正文两段。
const reasoningStream = "data: " + `{"id":"c1","model":"deepseek-r1","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n" +
	"data: " + `{"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"思考第一段"},"finish_reason":null}]}` + "\n\n" +
	"data: " + `{"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"思考第二段"},"finish_reason":null}]}` + "\n\n" +
	"data: " + `{"id":"c1","choices":[{"index":0,"delta":{"content":"正文"},"finish_reason":null}]}` + "\n\n" +
	"data: " + `{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}` + "\n\n" +
	"data: [DONE]\n\n"

func TestReasoningStreamToAnthropic(t *testing.T) {
	conv := NewStreamConverter(FormatOpenAI, FormatAnthropic, "deepseek-r1")
	out, err := conv.FeedChunk([]byte(reasoningStream))
	if err != nil {
		t.Fatalf("feed 失败: %v", err)
	}
	s := string(out)
	// thinking 块开闭与增量
	if !strings.Contains(s, `"type":"thinking"`) || !strings.Contains(s, `"thinking_delta"`) {
		t.Errorf("（流式 Anthropic）：应输出 thinking 块，实际: %s", s)
	}
	if !strings.Contains(s, `"thinking":"思考第一段"`) || !strings.Contains(s, `"thinking":"思考第二段"`) {
		t.Errorf("（流式 Anthropic）：思考增量丢失，实际: %s", s)
	}
	// thinking 块必须先于文本块关闭（块索引顺序）
	ti := strings.Index(s, `"thinking_delta"`)
	tt := strings.Index(s, `"type":"text_delta"`)
	if ti < 0 || tt < 0 || ti > tt {
		t.Errorf("（流式 Anthropic）：思考增量应先于正文，实际: %s", s)
	}
}

func TestReasoningStreamToResponses(t *testing.T) {
	conv := NewStreamConverter(FormatOpenAI, FormatResponses, "deepseek-r1")
	out, err := conv.FeedChunk([]byte(reasoningStream))
	if err != nil {
		t.Fatalf("feed 失败: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"type":"reasoning"`) || !strings.Contains(s, `"response.reasoning_text.delta"`) {
		t.Errorf("（流式 Responses）：应输出 reasoning 条目与增量事件，实际: %s", s)
	}
	if !strings.Contains(s, `"delta":"思考第一段"`) {
		t.Errorf("（流式 Responses）：思考增量丢失，实际: %s", s)
	}
}

func TestAnthropicThinkingStreamToCC(t *testing.T) {
	// Anthropic 上游 thinking 增量 → CC 客户端 reasoning_content
	stream := "data: " + `{"type":"message_start","message":{"id":"m1","model":"claude-3-7","usage":{"input_tokens":5}}}` + "\n\n" +
		"data: " + `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"data: " + `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先推理"}}` + "\n\n" +
		"data: " + `{"type":"content_block_stop","index":0}` + "\n\n" +
		"data: " + `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"data: " + `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"答案"}}` + "\n\n" +
		"data: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
		"data: " + `{"type":"message_stop"}` + "\n\n"
	conv := NewStreamConverter(FormatAnthropic, FormatOpenAI, "claude-3-7")
	out, err := conv.FeedChunk([]byte(stream))
	if err != nil {
		t.Fatalf("feed 失败: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"reasoning_content":"先推理"`) {
		t.Errorf("（流式 CC）：应输出 reasoning_content 增量，实际: %s", s)
	}
	if !strings.Contains(s, `"content":"答案"`) {
		t.Errorf("（流式 CC）：正文增量丢失，实际: %s", s)
	}
}

func TestReasoningPseudoSSE(t *testing.T) {
	// 伪流式：完整响应 → 客户端格式 SSE，思考内容应合成 thinking/reasoning 块
	up := []byte(`{"id":"c1","model":"deepseek-r1","choices":[{"message":{"role":"assistant","content":"答案","reasoning_content":"思考"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	ir, err := ParseResponse(FormatOpenAI, up)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	for _, client := range []string{FormatAnthropic, FormatResponses} {
		clientResp := BuildResponse(client, ir, ir.Model)
		out, err := BuildSSE(client, clientResp)
		if err != nil {
			t.Fatalf("（伪流式 %s）：BuildSSE 失败: %v", client, err)
		}
		if !strings.Contains(string(out), "思考") {
			t.Errorf("（伪流式 %s）：思考内容丢失，实际: %s", client, out)
		}
	}
}
