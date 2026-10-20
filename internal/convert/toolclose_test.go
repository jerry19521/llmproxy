// toolclose_test.go：工具调用条目收尾事件回归测试。
//
// 回归背景：OpenAI Chat Completions 上游的流式协议没有工具调用结束信号
// （工具结束由 finish_reason=tool_calls 隐式标记，解析器不产生 evToolEnd），
// 跨格式转换到 Responses / Anthropic 客户端时曾缺失收尾事件：
//   - Responses：缺 response.function_call_arguments.done / response.output_item.done，
//     且 response.completed 的 output 数组缺 function_call 条目
//   - Anthropic：tool_use 块缺 content_block_stop
//
// 官方客户端（如 Codex Desktop）严格校验事件序列，收到不完整流会解析失败并断开
// 连接（代理侧表现为「客户端断开」失败日志）。因此 finalize 统一补发 evToolEnd。
package convert

import (
	"strings"
	"testing"
)

// ccToolCallStream 模拟 OpenAI CC 上游流式响应（Codex/编码 agent 典型场景：
// 少量文本 + 工具调用 + finish_reason=tool_calls + [DONE]，无工具结束信号）。
const ccToolCallStream = `data: {"id":"c1","model":"MiniMax-M3","choices":[{"index":0,"delta":{"role":"assistant","content":"先看一下目录。"},"finish_reason":null}]}

data: {"id":"c1","model":"MiniMax-M3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_01a0","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":"}}]},"finish_reason":null}]}

data: {"id":"c1","model":"MiniMax-M3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls -la\"}"}}]},"finish_reason":null}]}

data: {"id":"c1","model":"MiniMax-M3","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`

// TestStreamCCToResponsesToolCallDone CC 上游 → Responses 客户端：
// 工具条目必须完整收尾，response.completed 的 output 必须包含 function_call 条目。
func TestStreamCCToResponsesToolCallDone(t *testing.T) {
	conv := NewStreamConverter(FormatOpenAI, FormatResponses, "MiniMax-M3")
	var out strings.Builder
	if b, err := conv.FeedChunk([]byte(ccToolCallStream)); err == nil {
		out.Write(b)
	}
	if fb, ferr := conv.Finish(); ferr == nil {
		out.Write(fb)
	}
	got := out.String()

	mustContain := []string{
		"response.output_item.added",             // 工具条目开始
		"response.function_call_arguments.delta", // 参数增量
		"response.function_call_arguments.done",  // 参数完成
		"response.output_item.done",              // 工具条目完成
		`"name":"exec_command"`,                  // 工具名透传
		`"arguments":"{\"cmd\":\"ls -la\"}"`,     // done/终态携带完整参数
		"response.completed",                     // 终态事件
	}
	for _, m := range mustContain {
		if !strings.Contains(got, m) {
			t.Errorf("输出应包含 %s\n--- 实际输出 ---\n%s", m, got)
		}
	}
	// response.completed 的 output 数组必须含 function_call 条目
	// （ e.items 仅在 evToolEnd 时追加，CC 上游永不触发 → output 只有 message）
	completedIdx := strings.LastIndex(got, "event: response.completed")
	if completedIdx < 0 {
		t.Fatalf("缺少 response.completed 事件")
	}
	tail := got[completedIdx:]
	if !strings.Contains(tail, `"type":"function_call"`) {
		t.Errorf("response.completed 的 output 数组应含 function_call 条目（Codex 严格校验，缺失会解析失败断连）\n--- completed 事件 ---\n%s", tail)
	}
	if !strings.Contains(tail, `"call_id":"call_01a0"`) {
		t.Errorf("function_call 条目应携带 call_id")
	}
	// done 事件的 arguments 为完整参数（增量拼接）
	if !strings.Contains(got, `"type":"response.function_call_arguments.done"`) {
		t.Errorf("缺少 function_call_arguments.done 事件")
	}
}

// TestStreamCCToAnthropicToolBlockStop CC 上游 → Anthropic 客户端：
// tool_use 块必须以 content_block_stop 收尾。
func TestStreamCCToAnthropicToolBlockStop(t *testing.T) {
	conv := NewStreamConverter(FormatOpenAI, FormatAnthropic, "MiniMax-M3")
	var out strings.Builder
	if b, err := conv.FeedChunk([]byte(ccToolCallStream)); err == nil {
		out.Write(b)
	}
	if fb, ferr := conv.Finish(); ferr == nil {
		out.Write(fb)
	}
	got := out.String()

	for _, m := range []string{
		"message_start",
		`"type":"tool_use"`,
		`"name":"exec_command"`,
		"input_json_delta",
		"message_stop",
	} {
		if !strings.Contains(got, m) {
			t.Errorf("输出应包含 %s\n--- 实际输出 ---\n%s", m, got)
		}
	}
	// tool_use 块（index≥1）必须有 content_block_stop
	// （ CC 上游无 evToolEnd → 工具块永不关闭，Anthropic SDK 解析失败）
	if c := strings.Count(got, "content_block_stop"); c < 2 {
		t.Errorf("文本块与工具块均应有 content_block_stop（工具块曾缺失），实际 %d 个\n--- 实际输出 ---\n%s", c, got)
	}
}

// TestStreamResponsesUpstreamToolEndUntouched Responses/Anthropic 上游自带 evToolEnd：
// 补发逻辑不得产生重复的结束事件。
func TestStreamResponsesUpstreamToolEndUntouched(t *testing.T) {
	// Responses 上游：function_call 条目自带 arguments.delta + arguments.done + output_item.done
	up := `data: {"type":"response.created","response":{"id":"resp_1","model":"m"}}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":""}}

data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"city\":"}

data: {"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"city\":\"上海\"}"}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"上海\"}","status":"completed"}}

data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}

`
	conv := NewStreamConverter(FormatResponses, FormatResponses, "m")
	var out strings.Builder
	if b, err := conv.FeedChunk([]byte(up)); err == nil {
		out.Write(b)
	}
	if fb, ferr := conv.Finish(); ferr == nil {
		out.Write(fb)
	}
	got := out.String()

	if c := strings.Count(got, `"type":"response.function_call_arguments.done"`); c != 1 {
		t.Errorf("arguments.done 应恰 1 个（不得重复补发），实际 %d\n%s", c, got)
	}
	if c := strings.Count(got, `"type":"response.output_item.done"`); c != 1 {
		t.Errorf("output_item.done 应恰 1 个（不得重复补发），实际 %d\n%s", c, got)
	}
}
