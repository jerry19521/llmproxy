package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// v1.7.6 修复回归：流式发射器与解析器的异常路径（M7 / M8）。

// TestAnthropicEmitterIndexUnique M7 回归：上游先发工具调用、后发文本（或文本→工具→文本
// 交替）时，Anthropic 客户端侧的 content_block_start 索引必须唯一且单调递增。
// 旧实现文本块硬编码 index 0，与工具块（从 nBlocks 顺序分配）冲突，
// 严格 Anthropic SDK 解析失败断连。
func TestAnthropicEmitterIndexUnique(t *testing.T) {
	conv := NewStreamConverter(FormatOpenAI, FormatAnthropic, "m")
	frames := []string{
		// 先工具调用
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]}}]}\n\n",
		// 工具后跟文本
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"说明文字\"}}]}\n\n",
		// 文本后再来一次工具
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":1,\"id\":\"call_2\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"\"}}]}}]}\n\n",
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
		"data: [DONE]\n\n",
	}
	var out []byte
	for _, f := range frames {
		o, err := conv.FeedChunk([]byte(f))
		if err != nil {
			t.Fatalf("FeedChunk 不应报错: %v", err)
		}
		out = append(out, o...)
	}
	if fin, ferr := conv.Finish(); ferr == nil {
		out = append(out, fin...)
	}

	var starts []float64
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var m map[string]interface{}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &m) != nil {
			continue
		}
		if m["type"] == "content_block_start" {
			idx, ok := m["index"].(float64)
			if !ok {
				t.Fatalf("content_block_start 缺 index: %v", m)
			}
			starts = append(starts, idx)
		}
	}
	if len(starts) < 3 {
		t.Fatalf("应产生至少 3 个内容块（文本×2 + 工具×2 的组合），实际 %d 个：%v", len(starts), starts)
	}
	seen := map[float64]bool{}
	for _, i := range starts {
		if seen[i] {
			t.Fatalf("content_block_start 索引重复: %v（旧 bug：文本块硬编码 index 0 与工具块冲突）", starts)
		}
		seen[i] = true
	}
	for i := 1; i < len(starts); i++ {
		if starts[i] <= starts[i-1] {
			t.Fatalf("索引必须单调递增: %v", starts)
		}
	}
}

// TestResponsesStreamErrorEvent M8 回归：Responses 上游流中 type=error 事件必须
// 归一为 evError（未开始写客户端时可重试），不得被静默丢弃并在 EOF 时伪造成功终止流。
func TestResponsesStreamErrorEvent(t *testing.T) {
	conv := NewStreamConverter(FormatResponses, FormatOpenAI, "m")
	frames := []string{
		"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\"}}\n\n",
		"event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"boom\"}\n\n",
	}
	var gotErr error
	for _, f := range frames {
		_, err := conv.FeedChunk([]byte(f))
		if err != nil {
			gotErr = err
		}
	}
	if gotErr == nil {
		t.Fatal("type=error 事件必须触发错误返回（旧 bug：无处理分支，错误被吞并在 EOF 伪造成功流）")
	}
	if !strings.Contains(gotErr.Error(), "boom") {
		t.Fatalf("错误消息应携带上游 message，实际: %v", gotErr)
	}
}

// TestResponsesStreamFailedEventDetail M8 回归：response.failed 事件的错误详情在
// response.error 内（官方协议），应正确提取而非退化为固定文案。
func TestResponsesStreamFailedEventDetail(t *testing.T) {
	conv := NewStreamConverter(FormatResponses, FormatOpenAI, "m")
	frames := []string{
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"r1\",\"status\":\"failed\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"quota exceeded\"}}}\n\n",
	}
	var gotErr error
	for _, f := range frames {
		_, err := conv.FeedChunk([]byte(f))
		if err != nil {
			gotErr = err
		}
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "quota exceeded") {
		t.Fatalf("response.failed 应提取 response.error.message，实际: %v", gotErr)
	}
}

// TestFallbackUsageJSONExtraction M5② 回归：伪流式兜底（上游对 stream=true 一次性返回
// 完整 JSON）时 usage 应可提取——SSE 模式提取全 0 时回退整体 JSON 提取。
func TestFallbackUsageJSONExtraction(t *testing.T) {
	body := []byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`)
	p, c, tm := ExtractUsage(body, true)
	if p == 0 && c == 0 {
		// SSE 模式对非 SSE 响应体返回 0 属预期；此处验证 JSON 模式可用即可
		p2, c2, t2 := ExtractUsage(body, false)
		if p2 != 10 || c2 != 20 || t2 != 30 {
			t.Fatalf("JSON 模式应提取 usage，实际 %d/%d/%d", p2, c2, t2)
		}
		return
	}
	if p != 10 || c != 20 || tm != 30 {
		t.Fatalf("usage 提取错误: %d/%d/%d", p, c, tm)
	}
}
