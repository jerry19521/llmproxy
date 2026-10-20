// stream_test.go：跨格式流式转换单元测试（SSE 扫描器 / 解析器 / 发射器 / 组合转换器）。
package convert

import (
	"strings"
	"testing"
)

func sseEvent(payload string) string { return "data: " + payload + "\n\n" }

// TestSSEScanner 帧 scanning：分块边界、\r\n、多 data 行拼接、流结束残余。
func TestSSEScanner(t *testing.T) {
	s := &sseScanner{}
	// 半个事件分两次 Feed
	got := s.feed([]byte("data: {\"a\":"))
	if len(got) != 0 {
		t.Fatalf("半个事件不应产出载荷: %v", got)
	}
	got = s.feed([]byte("1}\n\ndata: [DONE]\n"))
	if len(got) != 1 || got[0] != `{"a":1}` {
		t.Fatalf("分块拼接异常: %v", got)
	}
	// \r\n\r\n 分隔 + \r\n 行尾
	s2 := &sseScanner{}
	got = s2.feed([]byte("data: x\r\n\r\ndata: y\r\n\r\n"))
	if len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Fatalf("\\r\\n 分隔异常: %v", got)
	}
	// 多 data 行拼接（SSE 规范：多行以 \n 连接）
	s3 := &sseScanner{}
	got = s3.feed([]byte("data: line1\ndata: line2\n\n"))
	if len(got) != 1 || got[0] != "line1\nline2" {
		t.Fatalf("多 data 行拼接异常: %v", got)
	}
	// event: 行忽略
	s4 := &sseScanner{}
	got = s4.feed([]byte("event: message_start\ndata: {\"t\":1}\n\n"))
	if len(got) != 1 || got[0] != `{"t":1}` {
		t.Fatalf("event 行应忽略: %v", got)
	}
	// 流结束残余（未以空行结尾）
	s5 := &sseScanner{}
	s5.feed([]byte("data: a\n\n"))
	_ = s5.feed([]byte("data: tail"))
	rem := s5.remaining()
	if len(rem) != 1 || rem[0] != "tail" {
		t.Fatalf("残余提取异常: %v", rem)
	}
	// 空白残余返回空
	s6 := &sseScanner{}
	_ = s6.feed([]byte("event: ping\n"))
	if rem := s6.remaining(); len(rem) != 0 {
		t.Fatalf("无 data 行的残余应丢弃: %v", rem)
	}
}

// TestParseOpenAIStreamPayload OpenAI chunk 解析：text/tool 分片/finish/usage/error。
func TestParseOpenAIStreamPayload(t *testing.T) {
	// 首个 tool_calls 分片：start（chunk 元数据）+ id/name + 首段 arguments
	evs := ParseStreamPayload(FormatOpenAI, `{"id":"c1","model":"m1","choices":[{"index":0,
		"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":"{\"ci"}}]},"finish_reason":null}]}`)
	if len(evs) != 3 || evs[0].Kind != evStart || evs[1].Kind != evToolStart || evs[2].Kind != evToolArgs {
		t.Fatalf("tool 首分片解析异常: %+v", evs)
	}
	if evs[0].ID != "c1" || evs[0].Model != "m1" {
		t.Fatalf("start 字段异常: %+v", evs[0])
	}
	if evs[1].ToolID != "call_1" || evs[1].ToolName != "get_weather" || evs[1].ToolIndex != 0 {
		t.Fatalf("tool_start 字段异常: %+v", evs[1])
	}
	if evs[2].ToolArgs != `{"ci` {
		t.Fatalf("tool_args 字段异常: %+v", evs[2])
	}
	// 后续分片：仅 arguments 增量
	evs = ParseStreamPayload(FormatOpenAI, `{"choices":[{"index":0,
		"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":1}"}}]},"finish_reason":null}]}`)
	if len(evs) != 1 || evs[0].Kind != evToolArgs || evs[0].ToolArgs != `ty":1}` {
		t.Fatalf("tool 后续分片解析异常: %+v", evs)
	}
	// text + usage（chunk 无 id/model，不产生 start 事件）
	evs = ParseStreamPayload(FormatOpenAI, `{"choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}],"usage":{"prompt_tokens":5,"completion_tokens":9,"total_tokens":14}}`)
	if len(evs) != 2 {
		t.Fatalf("text/usage 解析数量异常: %+v", evs)
	}
	if evs[0].Kind != evUsage || evs[1].Kind != evText {
		t.Fatalf("text/usage 事件序异常: %+v", evs)
	}
	if evs[0].Completion != 9 || evs[1].Text != "你好" {
		t.Fatalf("字段异常: %+v", evs)
	}
	// finish_reason
	evs = ParseStreamPayload(FormatOpenAI, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
	if len(evs) != 1 || evs[0].Kind != evFinish || evs[0].Finish != "tool_calls" {
		t.Fatalf("finish 解析异常: %+v", evs)
	}
	// 错误事件
	evs = ParseStreamPayload(FormatOpenAI, `{"error":{"message":"model not found"}}`)
	if len(evs) != 1 || evs[0].Kind != evError || evs[0].Text != "model not found" {
		t.Fatalf("error 解析异常: %+v", evs)
	}
	// reasoning 字段归一为 evThinking（思考内容不再跨格式丢弃；
	// 同格式仍由代理层原样透传，不经此解析器）
	if evs := ParseStreamPayload(FormatOpenAI, `{"choices":[{"delta":{"reasoning":"思考中"},"finish_reason":null}]}`); len(evs) != 1 || evs[0].Kind != evThinking || evs[0].Text != "思考中" {
		t.Fatalf("reasoning 应归一为 thinking 事件: %+v", evs)
	}
}

// TestParseAnthropicStreamPayload Anthropic 事件解析。
func TestParseAnthropicStreamPayload(t *testing.T) {
	evs := ParseStreamPayload(FormatAnthropic, `{"type":"message_start","message":{"id":"msg_1","model":"claude-x","usage":{"input_tokens":66,"output_tokens":1}}}`)
	if len(evs) != 2 || evs[0].Kind != evUsage || evs[1].Kind != evStart {
		t.Fatalf("message_start 解析异常: %+v", evs)
	}
	if evs[0].Prompt != 66 || evs[1].ID != "msg_1" {
		t.Fatalf("字段异常: %+v", evs)
	}
	evs = ParseStreamPayload(FormatAnthropic, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}`)
	if len(evs) != 1 || evs[0].Kind != evText || evs[0].Text != "你好" {
		t.Fatalf("text_delta 解析异常: %+v", evs)
	}
	evs = ParseStreamPayload(FormatAnthropic, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"get_weather"}}`)
	if len(evs) != 1 || evs[0].Kind != evToolStart || evs[0].ToolIndex != 1 || evs[0].ToolID != "tu_1" {
		t.Fatalf("tool_use 解析异常: %+v", evs)
	}
	evs = ParseStreamPayload(FormatAnthropic, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\""}}`)
	if len(evs) != 1 || evs[0].Kind != evToolArgs || evs[0].ToolArgs != `{"a"` {
		t.Fatalf("input_json_delta 解析异常: %+v", evs)
	}
	evs = ParseStreamPayload(FormatAnthropic, `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":77}}`)
	if len(evs) != 2 || evs[0].Kind != evUsage || evs[0].Completion != 77 || evs[1].Kind != evFinish || evs[1].Finish != "length" {
		t.Fatalf("message_delta 解析异常: %+v", evs)
	}
	// ping 忽略
	if evs := ParseStreamPayload(FormatAnthropic, `{"type":"ping"}`); len(evs) != 0 {
		t.Fatalf("ping 应忽略: %+v", evs)
	}
}

// TestParseResponsesStreamPayload Responses 事件解析。
func TestParseResponsesStreamPayload(t *testing.T) {
	evs := ParseStreamPayload(FormatResponses, `{"type":"response.created","response":{"id":"resp_1","model":"gpt-x","status":"in_progress"}}`)
	if len(evs) != 1 || evs[0].Kind != evStart || evs[0].ID != "resp_1" {
		t.Fatalf("response.created 解析异常: %+v", evs)
	}
	evs = ParseStreamPayload(FormatResponses, `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"你好"}`)
	if len(evs) != 1 || evs[0].Kind != evText || evs[0].Text != "你好" {
		t.Fatalf("output_text.delta 解析异常: %+v", evs)
	}
	evs = ParseStreamPayload(FormatResponses, `{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"get_weather"}}`)
	if len(evs) != 1 || evs[0].Kind != evToolStart || evs[0].ToolIndex != 1 {
		t.Fatalf("function_call added 解析异常: %+v", evs)
	}
	evs = ParseStreamPayload(FormatResponses, `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":44,"output_tokens":55,"total_tokens":99}}}`)
	if len(evs) != 3 || evs[0].Kind != evUsage || evs[1].Kind != evFinish || evs[2].Kind != evDone {
		t.Fatalf("completed 解析异常: %+v", evs)
	}
	if evs[0].Completion != 55 || evs[1].Finish != "stop" {
		t.Fatalf("字段异常: %+v", evs)
	}
}

// runConverter 模拟逐块喂入上游 SSE 流并收集全部客户端输出。
func runConverter(t *testing.T, from, to, model, upstreamSSE string) string {
	t.Helper()
	c := NewStreamConverter(from, to, model)
	var sb strings.Builder
	// 按 7 字节分块喂入（验证任意切分边界）
	for i := 0; i < len(upstreamSSE); i += 7 {
		end := i + 7
		if end > len(upstreamSSE) {
			end = len(upstreamSSE)
		}
		out, err := c.FeedChunk([]byte(upstreamSSE[i:end]))
		sb.Write(out)
		if err != nil {
			t.Fatalf("FeedChunk 错误: %v", err)
		}
	}
	if fin, ferr := c.Finish(); ferr == nil {
		sb.Write(fin)
	}
	return sb.String()
}

// openAIStreamSSE 标准 OpenAI 流（text 两段 + tool 一个 + finish + usage + [DONE]）。
func openAIStreamSSE() string {
	return sseEvent(`{"id":"chatcmpl-1","model":"m-up","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`) +
		sseEvent(`{"id":"chatcmpl-1","model":"m-up","choices":[{"index":0,"delta":{"content":"今天"},"finish_reason":null}]}`) +
		sseEvent(`{"id":"chatcmpl-1","model":"m-up","choices":[{"index":0,"delta":{"content":"天气不错"},"finish_reason":null}]}`) +
		sseEvent(`{"id":"chatcmpl-1","model":"m-up","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`) +
		sseEvent(`{"id":"chatcmpl-1","model":"m-up","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"上海\"}"}}]},"finish_reason":null}]}`) +
		sseEvent(`{"id":"chatcmpl-1","model":"m-up","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`) +
		"data: [DONE]\n\n"
}

func anthropicStreamSSE() string {
	return "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_1","model":"claude-x","usage":{"input_tokens":66,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"今天"}}` + "\n\n" +
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"天气不错"}}` + "\n\n" +
		"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":77}}` + "\n\n" +
		"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
}

func responsesStreamSSE() string {
	return "event: response.created\ndata: " + `{"type":"response.created","response":{"id":"resp_1","model":"gpt-x","status":"in_progress"}}` + "\n\n" +
		"event: response.output_item.added\ndata: " + `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","status":"in_progress","role":"assistant"}}` + "\n\n" +
		"event: response.output_text.delta\ndata: " + `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"今天"}` + "\n\n" +
		"event: response.output_text.delta\ndata: " + `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"天气不错"}` + "\n\n" +
		"event: response.output_text.done\ndata: " + `{"type":"response.output_text.done","item_id":"msg_1","text":"今天天气不错"}` + "\n\n" +
		"event: response.output_item.done\ndata: " + `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","status":"completed"}}` + "\n\n" +
		"event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":44,"output_tokens":55,"total_tokens":99}}}` + "\n\n"
}

// TestStreamConverterOpenAIToAnthropic OpenAI 上游 → Anthropic 客户端。
func TestStreamConverterOpenAIToAnthropic(t *testing.T) {
	out := runConverter(t, FormatOpenAI, FormatAnthropic, "m-up", openAIStreamSSE())
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		`"text":"今天"`,
		`"text":"天气不错"`,
		"event: content_block_stop",
		`"type":"tool_use"`,
		`"partial_json":"{\"city\":"`,
		`"partial_json":"\"上海\"}"`,
		"event: message_delta",
		`"stop_reason":"tool_use"`, // finish=tool_calls → stop_reason=tool_use
		`"output_tokens":22`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %s:\n%s", want, out)
		}
	}
	// OpenAI 上游的 usage 在末 chunk 才到，而 Anthropic message_start 已发出 → input_tokens 记 0（设计限制），output 正确
	p, c, _ := ExtractUsage([]byte(out), true)
	if p != 0 || c != 22 {
		t.Errorf("usage 提取异常: p=%d c=%d", p, c)
	}
}

// TestStreamConverterOpenAIToResponses OpenAI 上游 → Responses 客户端。
func TestStreamConverterOpenAIToResponses(t *testing.T) {
	out := runConverter(t, FormatOpenAI, FormatResponses, "m-up", openAIStreamSSE())
	for _, want := range []string{
		"event: response.created",
		`"delta":"今天"`,
		`"delta":"天气不错"`,
		"event: response.output_item.done", // 文本条目关闭
		"event: response.function_call_arguments.delta",
		"event: response.completed",
		`"output_text":"今天天气不错"`,
		`"type":"function_call"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %s:\n%s", want, out)
		}
	}
	p, c, tt := ExtractUsage([]byte(out), true)
	if p != 11 || c != 22 || tt != 33 {
		t.Errorf("usage 提取异常: %d/%d/%d", p, c, tt)
	}
}

// TestStreamConverterAnthropicToOpenAI Anthropic 上游 → OpenAI 客户端。
func TestStreamConverterAnthropicToOpenAI(t *testing.T) {
	out := runConverter(t, FormatAnthropic, FormatOpenAI, "m-up", anthropicStreamSSE())
	for _, want := range []string{
		`"object":"chat.completion.chunk"`,
		`"content":"今天"`,
		`"content":"天气不错"`,
		`"finish_reason":"stop"`,
		"data: [DONE]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %s:\n%s", want, out)
		}
	}
	p, c, _ := ExtractUsage([]byte(out), true)
	if p != 66 || c != 77 {
		t.Errorf("usage 提取异常: p=%d c=%d", p, c)
	}
}

// TestStreamConverterAnthropicToResponses Anthropic 上游 → Responses 客户端。
func TestStreamConverterAnthropicToResponses(t *testing.T) {
	out := runConverter(t, FormatAnthropic, FormatResponses, "m-up", anthropicStreamSSE())
	for _, want := range []string{
		"event: response.created",
		"event: response.output_text.delta",
		`"delta":"今天"`,
		"event: response.completed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %s:\n%s", want, out)
		}
	}
	p, c, tt := ExtractUsage([]byte(out), true)
	if p != 66 || c != 77 || tt != 143 {
		t.Errorf("usage 提取异常: %d/%d/%d", p, c, tt)
	}
}

// TestStreamConverterResponsesToOpenAI Responses 上游 → OpenAI 客户端。
func TestStreamConverterResponsesToOpenAI(t *testing.T) {
	out := runConverter(t, FormatResponses, FormatOpenAI, "m-up", responsesStreamSSE())
	for _, want := range []string{
		`"content":"今天"`,
		`"content":"天气不错"`,
		`"finish_reason":"stop"`,
		"data: [DONE]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %s:\n%s", want, out)
		}
	}
	p, c, _ := ExtractUsage([]byte(out), true)
	if p != 44 || c != 55 {
		t.Errorf("usage 提取异常: p=%d c=%d", p, c)
	}
}

// TestStreamConverterResponsesToAnthropic Responses 上游 → Anthropic 客户端。
func TestStreamConverterResponsesToAnthropic(t *testing.T) {
	out := runConverter(t, FormatResponses, FormatAnthropic, "m-up", responsesStreamSSE())
	for _, want := range []string{
		"event: message_start",
		`"text":"今天"`,
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %s:\n%s", want, out)
		}
	}
	// Responses 上游的 usage 在末尾 completed 事件，Anthropic message_start 已发出 → input_tokens 记 0（设计限制），output 正确
	p, c, _ := ExtractUsage([]byte(out), true)
	if p != 0 || c != 55 {
		t.Errorf("usage 提取异常: p=%d c=%d", p, c)
	}
}

// TestStreamConverterUpstreamError 流中错误事件。
func TestStreamConverterUpstreamError(t *testing.T) {
	c := NewStreamConverter(FormatOpenAI, FormatAnthropic, "m")
	out, err := c.FeedChunk([]byte("data: {\"error\":{\"message\":\"model overloaded\"}}\n\n"))
	if err == nil {
		t.Fatal("错误事件应返回 UpstreamStreamError")
	}
	if !strings.Contains(err.Error(), "model overloaded") {
		t.Errorf("错误消息异常: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("首事件为错误时不应有输出: %s", out)
	}
}

// TestStreamConverterEmptyStream 空流：Finish 仍输出完整合法事件序列。
func TestStreamConverterEmptyStream(t *testing.T) {
	c := NewStreamConverter(FormatOpenAI, FormatAnthropic, "m")
	if _, err := c.FeedChunk([]byte("")); err != nil {
		t.Fatalf("空输入不应报错: %v", err)
	}
	fin, ferr := c.Finish()
	if ferr != nil {
		t.Fatalf("空流收尾不应报错: %v", ferr)
	}
	if !strings.Contains(string(fin), "event: message_start") ||
		!strings.Contains(string(fin), "event: message_stop") {
		t.Errorf("空流收尾应包含完整事件序列: %s", fin)
	}
}

// TestStreamConverterNoFinishEOF 上游流结束但未显式发 finish 事件：默认 stop 收尾。
func TestStreamConverterNoFinishEOF(t *testing.T) {
	c := NewStreamConverter(FormatOpenAI, FormatAnthropic, "m")
	var all []byte
	out, _ := c.FeedChunk([]byte(sseEvent(`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`) + "data: [DONE]\n\n"))
	all = append(all, out...)
	fin, ferr := c.Finish()
	if ferr != nil {
		t.Fatalf("正常 EOF 收尾不应报错: %v", ferr)
	}
	all = append(all, fin...)
	if !strings.Contains(string(all), `"stop_reason":"end_turn"`) {
		t.Errorf("未显式 finish 应默认 stop: %s", all)
	}
}

// TestFinishDeferredUntilDone 终止帧必须延迟到流的真正结束信号（回归测试）。
// OpenAI CC 流的 finish_reason chunk 之后还有 usage chunk（stream_options.include_usage）
// 与 [DONE]：若在 finish_reason 时就发出终止帧，终止帧里的 usage 全为 0，且客户端收到
// 终止帧即正常关闭连接后，代理还在等上游剩余数据，会把正常完成误判为"客户端断开"。
func TestFinishDeferredUntilDone(t *testing.T) {
	c := NewStreamConverter(FormatOpenAI, FormatResponses, "m")

	// finish_reason chunk：只记录，不得终止
	out1, err := c.FeedChunk([]byte(sseEvent(`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)))
	if err != nil {
		t.Fatalf("finish chunk: %v", err)
	}
	if c.Finished() {
		t.Fatal("收到 finish_reason 不应立即终止")
	}
	if strings.Contains(string(out1), "response.completed") {
		t.Fatalf("终止帧不得在 finish_reason 时发出: %s", out1)
	}
	_ = out1

	// usage chunk（finish 之后、[DONE] 之前到达）：更新用量，仍不终止
	out2, err := c.FeedChunk([]byte(sseEvent(`{"id":"c1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`)))
	if err != nil {
		t.Fatalf("usage chunk: %v", err)
	}
	if c.Finished() {
		t.Fatal("usage chunk 后仍不应终止")
	}
	if strings.Contains(string(out2), "response.completed") {
		t.Fatalf("usage chunk 阶段不应输出终止帧: %s", out2)
	}

	// [DONE]：此刻终止，终止帧须带上正确的 usage
	out3, err := c.FeedChunk([]byte("data: [DONE]\n\n"))
	if err != nil {
		t.Fatalf("[DONE]: %v", err)
	}
	if !c.Finished() {
		t.Fatal("[DONE] 后应已终止")
	}
	for _, want := range []string{
		"event: response.completed",
		`"input_tokens":100`,
		`"output_tokens":20`,
		`"total_tokens":120`,
	} {
		if !strings.Contains(string(out3), want) {
			t.Errorf("终止帧缺少 %s:\n%s", want, out3)
		}
	}

	// 幂等：[DONE] 后 Finish() 不再产生输出
	if fin, ferr := c.Finish(); ferr == nil && len(fin) != 0 {
		t.Errorf("终止后 Finish() 应幂等返回空, 实际: %s", fin)
	}
}

// TestFinishDeferredAnthropicUpstream Anthropic 上游：message_delta(含 stop_reason 与 usage)
// 只记录不终止，终止帧延迟到 message_stop（此时 usage 已完整）。
func TestFinishDeferredAnthropicUpstream(t *testing.T) {
	c := NewStreamConverter(FormatAnthropic, FormatOpenAI, "m")
	// message_delta：stop_reason + output_tokens——不终止
	_, err := c.FeedChunk([]byte(sseEvent(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`)))
	if err != nil {
		t.Fatalf("message_delta: %v", err)
	}
	if c.Finished() {
		t.Fatal("message_delta 不应立即终止")
	}
	// message_stop：终止，帧含 finish 与 usage
	out2, err := c.FeedChunk([]byte(sseEvent(`{"type":"message_stop"}`)))
	if err != nil {
		t.Fatalf("message_stop: %v", err)
	}
	if !c.Finished() {
		t.Fatal("message_stop 后应已终止")
	}
	for _, want := range []string{`"finish_reason":"stop"`, `"completion_tokens":42`, "data: [DONE]"} {
		if !strings.Contains(string(out2), want) {
			t.Errorf("终止帧缺少 %s:\n%s", want, out2)
		}
	}
}
