package convert

import (
	"encoding/json"
	"testing"
)

// 回归测试：Responses 文本块累积 / SSE 多行 data usage 提取。

// parseSSEResponsesOut 把转换器输出的 SSE 字节解析为事件 JSON 列表（按出现顺序）。
// 复用包内私有 sseScanner，与生产解析路径同口径。
func parseSSEResponsesOut(t *testing.T, out []byte) []map[string]json.RawMessage {
	t.Helper()
	var sc sseScanner
	payloads := sc.feed(out)
	payloads = append(payloads, sc.remaining()...)
	var evs []map[string]json.RawMessage
	for _, p := range payloads {
		if p == "[DONE]" {
			continue
		}
		var ev map[string]json.RawMessage
		if json.Unmarshal([]byte(p), &ev) == nil {
			evs = append(evs, ev)
		}
	}
	return evs
}

// TestResponsesTextBlockResetOnToolInterleave 文本→工具→文本交替流，
// Responses 发射器 closeTextItem 应按块重置 textAccum——
// output_text.done 序列为 [A, B]（因未重置为 [A, AB]）；
// completed.output_text 由 totalText 跨块累计为 "AB"。
func TestResponsesTextBlockResetOnToolInterleave(t *testing.T) {
	conv := NewStreamConverter(FormatOpenAI, FormatResponses, "m")
	stream := "" +
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"A\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"B\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n" +
		"data: [DONE]\n\n"
	var out []byte
	b, _ := conv.FeedChunk([]byte(stream))
	out = append(out, b...)
	if fin, ferr := conv.Finish(); ferr == nil {
		out = append(out, fin...)
	}

	evs := parseSSEResponsesOut(t, out)
	var doneTexts []string
	var completedText string
	for _, ev := range evs {
		var ty string
		_ = json.Unmarshal(ev["type"], &ty)
		switch ty {
		case "response.output_text.done":
			var text string
			_ = json.Unmarshal(ev["text"], &text)
			doneTexts = append(doneTexts, text)
		case "response.completed", "response.incomplete":
			var r struct {
				OutputText string `json:"output_text"`
			}
			_ = json.Unmarshal(ev["response"], &r)
			completedText = r.OutputText
		}
	}
	if len(doneTexts) != 2 || doneTexts[0] != "A" || doneTexts[1] != "B" {
		t.Fatalf("output_text.done 应为 [A B]（未重置 textAccum 误为 [A AB]），实际 %q", doneTexts)
	}
	if completedText != "AB" {
		t.Fatalf("completed.output_text 应为 AB（totalText 跨块累计），实际 %q", completedText)
	}
}

// TestExtractUsageMultilineDataFrames SSE 规范允许一个事件的载荷分多行 data: 传输，
// 按规范以 \n 拼接后解析；逐行独立解析导致多行帧全部解析失败、usage 误记 0。
func TestExtractUsageMultilineDataFrames(t *testing.T) {
	multi := "data: {\"usage\":\ndata: {\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n"
	p, c, total := ExtractUsage([]byte(multi), true)
	if p != 10 || c != 5 || total != 15 {
		t.Fatalf("多行 data: 帧 usage 应为 (10,5,15)，实际 (%d,%d,%d)", p, c, total)
	}
	// CRLF 归一化兼容：\r\n\r\n 事件分隔 + \r\n 帧内多行
	multiCRLF := "data: {\"usage\":\r\ndata: {\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\r\n\r\n"
	p2, c2, t2 := ExtractUsage([]byte(multiCRLF), true)
	if p2 != 10 || c2 != 5 || t2 != 15 {
		t.Fatalf("CRLF 多行 data: 帧 usage 应为 (10,5,15)，实际 (%d,%d,%d)", p2, c2, t2)
	}
	// 单行对照
	single := "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n"
	p3, c3, t3 := ExtractUsage([]byte(single), true)
	if p3 != 10 || c3 != 5 || t3 != 15 {
		t.Fatalf("单行 data: 帧 usage 应为 (10,5,15)，实际 (%d,%d,%d)", p3, c3, t3)
	}
}
