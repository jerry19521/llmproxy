package convert

// 回归测试：
//   responsesSSE 兜底合成首事件为 response.created（created/in_progress 前置于条目事件）
//   parseResponsesResponse 解析 reasoning 条目 summary 通道（content 优先、按条目独立兜底）

import (
	"bytes"
	"strings"
	"testing"
)

// firstSSEEvent 提取合成 SSE 的首个事件名（sseFrame 输出 "event: <name>"）。
func firstSSEEvent(sse []byte) string {
	i := bytes.Index(sse, []byte("event: "))
	if i < 0 {
		return ""
	}
	rest := sse[i+len("event: "):]
	j := bytes.IndexByte(rest, '\n')
	if j < 0 {
		return ""
	}
	return string(rest[:j])
}

// TestBuildSSEResponsesFirstEventCreated 上游忽略 stream 参数返回完整
// JSON 时（fallbackFullBodySSE 路径），带 reasoning 条目的响应合成 Responses SSE
// 的首事件必须为 response.created。若 reasoning 条目事件先于 created 写入
// （首事件为 response.output_item.added），则违反官方事件序列。
func TestBuildSSEResponsesFirstEventCreated(t *testing.T) {
	body := []byte(`{
		"id":"resp_1","object":"response","created_at":1760000000,"status":"completed","model":"gpt-5",
		"output":[
			{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"thinking..."}]},
			{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}
		],
		"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3},"output_text":"hello"}`)
	sse, err := BuildSSE(FormatResponses, body)
	if err != nil {
		t.Fatal(err)
	}
	if first := firstSSEEvent(sse); first != "response.created" {
		t.Fatalf("首事件应 response.created，实际 %q——reasoning 条目事件先于 created 写入，违反官方序列", first)
	}
	// 序列完整性：created → in_progress → reasoning 条目 → 正文 → 终态
	s := string(sse)
	idxCreated := strings.Index(s, "response.created")
	idxInProgress := strings.Index(s, "response.in_progress")
	idxReasoning := strings.Index(s, "response.reasoning_text.delta")
	idxText := strings.Index(s, "response.output_text.delta")
	if !(idxCreated < idxInProgress && idxInProgress < idxReasoning && idxReasoning < idxText) {
		t.Fatalf("事件顺序应 created→in_progress→reasoning→正文，实际 created=%d in_progress=%d reasoning=%d text=%d",
			idxCreated, idxInProgress, idxReasoning, idxText)
	}
	// 终态事件含完整 output 与 usage（reasoning 条目不因顺序调整而丢失）
	if !strings.Contains(s, `"input_tokens":1`) || !strings.Contains(s, "response.completed") {
		t.Fatalf("终态事件应含 usage 与 completed")
	}
}

// TestBuildSSEResponsesFirstEventCreatedNoReasoning 对照组：无 reasoning 条目时
// 首事件同样为 response.created（既有行为不回归）。
func TestBuildSSEResponsesFirstEventCreatedNoReasoning(t *testing.T) {
	body := []byte(`{
		"id":"resp_2","object":"response","created_at":1760000000,"status":"completed","model":"gpt-4o",
		"output":[
			{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}
		],
		"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3},"output_text":"hi"}`)
	sse, err := BuildSSE(FormatResponses, body)
	if err != nil {
		t.Fatal(err)
	}
	if first := firstSSEEvent(sse); first != "response.created" {
		t.Fatalf("对照组：首事件应 response.created，实际 %q", first)
	}
}

// TestParseResponsesReasoningSummary reasoning 条目思考内容的通道解析。
// 解析器须同时识别 content[].reasoning_text 与 summary 数组：官方非流式响应
// 的 reasoning 常仅有 summary 数组（content 为显式开启输出模式后的新增通道），
// 只认前者会导致思考内容静默丢失。
func TestParseResponsesReasoningSummary(t *testing.T) {
	// 场景 1：仅 summary 通道（官方非流式常见形态）
	summaryOnly := []byte(`{"id":"r1","object":"response","status":"completed","model":"o3",
		"output":[
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"step-by-step"}]},
			{"type":"message","id":"m1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer"}]}
		],
		"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)
	ir, err := ParseResponse(FormatResponses, summaryOnly)
	if err != nil {
		t.Fatal(err)
	}
	if ir.Reasoning != "step-by-step" {
		t.Fatalf("仅 summary 通道时 Reasoning 应为思考摘要，实际 %q（静默丢失）", ir.Reasoning)
	}
	if ir.Text != "answer" {
		t.Fatalf("正文解析不应受影响，实际 %q", ir.Text)
	}

	// 场景 2：content 优先——content 有 reasoning_text 时不取 summary
	both := []byte(`{"id":"r2","object":"response","status":"completed","model":"o3",
		"output":[{"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"S"}],"content":[{"type":"reasoning_text","text":"C"}]}],
		"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	ir2, err := ParseResponse(FormatResponses, both)
	if err != nil {
		t.Fatal(err)
	}
	if ir2.Reasoning != "C" {
		t.Fatalf("content 通道优先，Reasoning 应为 C，实际 %q", ir2.Reasoning)
	}

	// 场景 3：多 reasoning 条目——第一条 content 命中、第二条仅 summary，
	// 兜底判定按条目独立进行（全局判定会漏掉第二条的 summary）
	multi := []byte(`{"id":"r3","object":"response","status":"completed","model":"o3",
		"output":[
			{"type":"reasoning","id":"rs_a","summary":[],"content":[{"type":"reasoning_text","text":"thinkingA"}]},
			{"type":"reasoning","id":"rs_b","summary":[{"type":"summary_text","text":"sumB"}]}
		],
		"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	ir3, err := ParseResponse(FormatResponses, multi)
	if err != nil {
		t.Fatal(err)
	}
	if ir3.Reasoning != "thinkingAsumB" {
		t.Fatalf("兜底应按条目独立判定，Reasoning 应为 thinkingAsumB，实际 %q", ir3.Reasoning)
	}
}
