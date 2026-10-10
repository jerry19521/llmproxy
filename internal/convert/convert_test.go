package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// sampleIR 构造覆盖全特性的请求 IR：系统词 + 多模态 user + assistant 工具调用 + 工具结果。
func sampleIR() *RequestIR {
	temp := 0.7
	topP := 0.9
	return &RequestIR{
		Model:       "test-model",
		System:      "你是一个测试助手",
		MaxTokens:   1024,
		Temperature: &temp,
		TopP:        &topP,
		Stop:        []string{"END"},
		Tools: []ToolDef{{
			Name:        "get_weather",
			Description: "查询天气",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
		ToolChoice:      &ToolChoice{Type: "auto"},
		ReasoningEffort: "high",
		Messages: []InputMessage{
			{Role: "user", Parts: []ContentPart{
				{Type: "text", Text: "看这张图"},
				{Type: "image", ImageB64: "aGVsbG8=", ImageMime: "image/png"},
			}},
			{Role: "assistant", Text: "好的，我来查询。",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "get_weather", Args: `{"city":"上海"}`}}},
			{Role: "tool", ToolCallID: "call_1", Text: `{"temp":25}`},
			{Role: "user", Text: "谢谢，总结一下"},
		},
	}
}

// TestRequestRoundTripAllPairs 三种格式两两互转：Build → Parse → 校验关键字段。
func TestRequestRoundTripAllPairs(t *testing.T) {
	formats := APITypes()
	for _, from := range formats {
		for _, to := range formats {
			ir := sampleIR()
			body, err := BuildRequest(to, ir, "real-model")
			if err != nil {
				t.Fatalf("%s→%s build: %v", from, to, err)
			}
			parsed, err := ParseRequest(to, body)
			if err != nil {
				t.Fatalf("%s→%s parse: %v\nbody: %s", from, to, err, body)
			}
			// 公共断言
			if parsed.Model != "real-model" {
				t.Errorf("%s→%s model = %q, want real-model", from, to, parsed.Model)
			}
			if parsed.System != "你是一个测试助手" {
				t.Errorf("%s→%s system = %q", from, to, parsed.System)
			}
			if parsed.MaxTokens != 1024 {
				t.Errorf("%s→%s maxTokens = %d", from, to, parsed.MaxTokens)
			}
			if parsed.Temperature == nil || *parsed.Temperature != 0.7 {
				t.Errorf("%s→%s temperature = %v", from, to, parsed.Temperature)
			}
			if len(parsed.Tools) != 1 || parsed.Tools[0].Name != "get_weather" {
				t.Errorf("%s→%s tools = %+v", from, to, parsed.Tools)
			}
			if len(parsed.Messages) != 4 {
				t.Errorf("%s→%s messages = %d 条, want 4\n%s", from, to, len(parsed.Messages), body)
			}
			// assistant 工具调用
			var asst *InputMessage
			for i := range parsed.Messages {
				if parsed.Messages[i].Role == "assistant" {
					asst = &parsed.Messages[i]
				}
			}
			if asst == nil || len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_1" ||
				asst.ToolCalls[0].Name != "get_weather" || !strings.Contains(asst.ToolCalls[0].Args, "上海") {
				t.Errorf("%s→%s assistant tool_calls 异常: %+v", from, to, asst)
			}
			if asst != nil && asst.Text == "" {
				t.Errorf("%s→%s assistant text 丢失", from, to)
			}
			// tool 结果
			var toolMsg *InputMessage
			for i := range parsed.Messages {
				if parsed.Messages[i].Role == "tool" {
					toolMsg = &parsed.Messages[i]
				}
			}
			if toolMsg == nil || toolMsg.ToolCallID != "call_1" || !strings.Contains(toolMsg.Text, "25") {
				t.Errorf("%s→%s tool 消息异常: %+v", from, to, toolMsg)
			}
			// 首条 user 多模态（Responses/Anthropic 转换后 text 片段保留）
			first := parsed.Messages[0]
			if first.Role != "user" {
				t.Errorf("%s→%s 首条消息 role = %s, want user", from, to, first.Role)
			}
			hasText := first.Text != "" || len(first.Parts) > 0
			if !hasText {
				t.Errorf("%s→%s 首条 user 内容丢失", from, to)
			}
		}
	}
}

// TestAnthropicRequestRequiresMaxTokens 转换为 Anthropic 时 max_tokens 默认 4096。
func TestAnthropicRequestRequiresMaxTokens(t *testing.T) {
	ir := &RequestIR{Model: "m", Messages: []InputMessage{{Role: "user", Text: "hi"}}}
	body, err := BuildRequest(FormatAnthropic, ir, "m")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	_ = json.Unmarshal(body, &m)
	if m["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v, want 4096", m["max_tokens"])
	}
	// 相邻 tool 消息合并为一条 user（tool_result 块）
	ir2 := sampleIR()
	ir2.Messages = append(ir2.Messages, InputMessage{Role: "tool", ToolCallID: "call_2", Text: "r2"})
	body2, _ := BuildRequest(FormatAnthropic, ir2, "m")
	var m2 struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body2, &m2)
	toolResultCount := 0
	for _, msg := range m2.Messages {
		var blocks []map[string]interface{}
		_ = json.Unmarshal(msg.Content, &blocks)
		for _, b := range blocks {
			if b["type"] == "tool_result" {
				toolResultCount++
			}
		}
	}
	if toolResultCount != 2 {
		t.Errorf("tool_result 块数量 = %d, want 2（两条 tool 消息合并进 user）", toolResultCount)
	}
}

// TestResponseCrossConversions 6 条跨格式响应转换路径。
func TestResponseCrossConversions(t *testing.T) {
	ccResp := []byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":"gpt-x",
		"choices":[{"index":0,"message":{"role":"assistant","content":"你好","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"上海\"}"}}]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`)
	respResp := []byte(`{"id":"resp_1","object":"response","created_at":1700000000,"status":"completed","model":"gpt-x",
		"output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"你好","annotations":[]}]},
			{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"上海\"}"}],
		"usage":{"input_tokens":11,"output_tokens":22,"total_tokens":33}}`)
	antResp := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",
		"content":[{"type":"text","text":"你好"},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"上海"}}],
		"stop_reason":"tool_use","usage":{"input_tokens":11,"output_tokens":22}}`)

	check := func(t *testing.T, tag string, ir *ResponseIR) {
		if ir.Text != "你好" {
			t.Errorf("%s text = %q", tag, ir.Text)
		}
		if len(ir.ToolCalls) != 1 || ir.ToolCalls[0].Name != "get_weather" ||
			!strings.Contains(ir.ToolCalls[0].Args, "上海") {
			t.Errorf("%s toolCalls = %+v", tag, ir.ToolCalls)
		}
		if ir.PromptTokens != 11 || ir.CompletionTokens != 22 {
			t.Errorf("%s usage = %d/%d", tag, ir.PromptTokens, ir.CompletionTokens)
		}
	}

	irCC, err := ParseResponse(FormatOpenAI, ccResp)
	if err != nil {
		t.Fatal(err)
	}
	check(t, "cc-parse", irCC)
	if irCC.Finish != "tool_calls" {
		t.Errorf("cc finish = %s", irCC.Finish)
	}
	irR, err := ParseResponse(FormatResponses, respResp)
	if err != nil {
		t.Fatal(err)
	}
	check(t, "responses-parse", irR)
	irA, err := ParseResponse(FormatAnthropic, antResp)
	if err != nil {
		t.Fatal(err)
	}
	check(t, "anthropic-parse", irA)
	if irA.Finish != "tool_calls" {
		t.Errorf("anthropic finish = %s", irA.Finish)
	}

	// 构建为其他格式再解析回来
	for _, target := range APITypes() {
		body := BuildResponse(target, irCC, irCC.Model)
		back, err := ParseResponse(target, body)
		if err != nil {
			t.Fatalf("build→%s→parse: %v\n%s", target, err, body)
		}
		check(t, "cc→"+target, back)
	}
	for _, target := range APITypes() {
		body := BuildResponse(target, irA, irA.Model)
		back, err := ParseResponse(target, body)
		if err != nil {
			t.Fatalf("anthropic→%s: %v", target, err)
		}
		check(t, "anthropic→"+target, back)
	}
}

// TestUpstreamURL 各类型上游 URL 拼接。
func TestUpstreamURL(t *testing.T) {
	cases := []struct{ apiType, base, want string }{
		{"openai", "https://api.openai.com/v1", "https://api.openai.com/v1/chat/completions"},
		{"", "https://api.openai.com/v1/", "https://api.openai.com/v1/chat/completions"},
		{"responses", "https://api.openai.com/v1", "https://api.openai.com/v1/responses"},
		{"anthropic", "https://api.anthropic.com", "https://api.anthropic.com/v1/messages"},
		{"anthropic", "https://api.anthropic.com/v1", "https://api.anthropic.com/v1/messages"},
		{"anthropic", "https://proxy.example.com/anthropic", "https://proxy.example.com/anthropic/v1/messages"},
	}
	for _, c := range cases {
		if got := UpstreamURL(c.apiType, c.base); got != c.want {
			t.Errorf("UpstreamURL(%q,%q) = %q, want %q", c.apiType, c.base, got, c.want)
		}
	}
	if got := ModelsURL("anthropic", "https://api.anthropic.com"); got != "https://api.anthropic.com/v1/models" {
		t.Errorf("ModelsURL anthropic = %q", got)
	}
	if got := ModelsURL("openai", "https://api.openai.com/v1"); got != "https://api.openai.com/v1/models" {
		t.Errorf("ModelsURL openai = %q", got)
	}
}

// TestSSESynthesis 三种格式 SSE 合成。
func TestSSESynthesis(t *testing.T) {
	ir := &ResponseIR{
		ID: "x1", Model: "m", Created: 1700000000, Text: "你好世界",
		ToolCalls: []ToolCall{{ID: "call_1", Name: "f", Args: `{"a":1}`}},
		Finish:    "tool_calls", PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12,
	}
	for _, format := range APITypes() {
		complete := BuildResponse(format, ir, ir.Model)
		sse, err := BuildSSE(format, complete)
		if err != nil {
			t.Fatalf("%s SSE: %v", format, err)
		}
		s := string(sse)
		if !strings.Contains(s, "data: ") {
			t.Errorf("%s SSE 缺少 data 帧", format)
		}
		if format == FormatOpenAI && !strings.Contains(s, "[DONE]") {
			t.Errorf("openai SSE 缺少 [DONE]")
		}
		if format == FormatAnthropic && !strings.Contains(s, "event: message_start") {
			t.Errorf("anthropic SSE 缺少 message_start")
		}
		if format == FormatAnthropic && !strings.Contains(s, "event: message_stop") {
			t.Errorf("anthropic SSE 缺少 message_stop")
		}
		if format == FormatResponses && !strings.Contains(s, "event: response.completed") {
			t.Errorf("responses SSE 缺少 response.completed")
		}
		if !strings.Contains(s, "你好世界") {
			t.Errorf("%s SSE 缺少正文文本", format)
		}
		// usage 提取
		p, c, total := ExtractUsage(sse, true)
		if p != 5 || c != 7 {
			t.Errorf("%s SSE usage = %d/%d, want 5/7", format, p, c)
		}
		_ = total
	}
}

// TestExtractUsage 各格式非流式 usage 提取。
func TestExtractUsage(t *testing.T) {
	cases := []struct {
		tag         string
		body        string
		p, c, total int64
	}{
		{"openai", `{"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`, 10, 20, 30},
		{"responses", `{"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}`, 10, 20, 30},
		{"anthropic", `{"usage":{"input_tokens":10,"output_tokens":20}}`, 10, 20, 30},
		{"none", `{"foo":1}`, 0, 0, 0},
	}
	for _, c := range cases {
		p, cc, t2 := ExtractUsage([]byte(c.body), false)
		if p != c.p || cc != c.c || t2 != c.total {
			t.Errorf("%s usage = %d/%d/%d, want %d/%d/%d", c.tag, p, cc, t2, c.p, c.c, c.total)
		}
	}
	// Anthropic 流式（message_start + message_delta）
	ant := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":4}}\n\n"
	p, c, _ := ExtractUsage([]byte(ant), true)
	if p != 9 || c != 4 {
		t.Errorf("anthropic SSE usage = %d/%d, want 9/4", p, c)
	}
	// Responses 流式（response.completed 内嵌 usage）
	rs := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":6,\"total_tokens\":14}}}\n\n"
	p, c, _ = ExtractUsage([]byte(rs), true)
	if p != 8 || c != 6 {
		t.Errorf("responses SSE usage = %d/%d, want 8/6", p, c)
	}
}

// TestParseRequestErrors 非法请求体报错。
func TestParseRequestErrors(t *testing.T) {
	if _, err := ParseRequest(FormatOpenAI, []byte(`{invalid`)); err == nil {
		t.Error("openai 非法 JSON 应报错")
	}
	if _, err := ParseRequest(FormatAnthropic, []byte(`{invalid`)); err == nil {
		t.Error("anthropic 非法 JSON 应报错")
	}
	if _, err := ParseRequest(FormatResponses, []byte(`{invalid`)); err == nil {
		t.Error("responses 非法 JSON 应报错")
	}
}

// TestImageConversion 图片在三种格式间转换（data URL ↔ base64 source ↔ input_image）。
func TestImageConversion(t *testing.T) {
	// OpenAI 格式含 data URL 图片 → Anthropic → Responses → 回到 OpenAI
	cc := []byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"图"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,QUJD"}}]}]}`)
	ir, err := ParseRequest(FormatOpenAI, cc)
	if err != nil {
		t.Fatal(err)
	}
	if len(ir.Messages[0].Parts) != 2 || ir.Messages[0].Parts[1].ImageB64 != "QUJD" || ir.Messages[0].Parts[1].ImageMime != "image/jpeg" {
		t.Fatalf("openai 图片解析异常: %+v", ir.Messages[0].Parts)
	}
	antBody, _ := BuildRequest(FormatAnthropic, ir, "m")
	var ant struct {
		Messages []struct {
			Content []struct {
				Type   string `json:"type"`
				Source *struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(antBody, &ant)
	if len(ant.Messages) == 0 || len(ant.Messages[0].Content) < 2 ||
		ant.Messages[0].Content[1].Source.Data != "QUJD" || ant.Messages[0].Content[1].Source.MediaType != "image/jpeg" {
		t.Fatalf("anthropic 图片块异常: %s", antBody)
	}
	respBody, _ := BuildRequest(FormatResponses, ir, "m")
	var rs struct {
		Input []struct {
			Content []struct {
				Type     string `json:"type"`
				ImageURL string `json:"image_url"`
			} `json:"content"`
		} `json:"input"`
	}
	_ = json.Unmarshal(respBody, &rs)
	if len(rs.Input) == 0 || len(rs.Input[0].Content) < 2 ||
		rs.Input[0].Content[1].ImageURL != "data:image/jpeg;base64,QUJD" {
		t.Fatalf("responses 图片片段异常: %s", respBody)
	}
}

// TestOpenAICompatStringInput Responses/Anthropic 字符串输入解析。
func TestOpenAICompatStringInput(t *testing.T) {
	// Responses input 为纯字符串
	ir, err := ParseRequest(FormatResponses, []byte(`{"model":"m","input":"你好"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(ir.Messages) != 1 || ir.Messages[0].Text != "你好" {
		t.Fatalf("responses 字符串 input 解析异常: %+v", ir.Messages)
	}
	// Anthropic system 为块数组
	ir2, err := ParseRequest(FormatAnthropic, []byte(`{"model":"m","system":[{"type":"text","text":"S1"},{"type":"text","text":"S2"}],"messages":[{"role":"user","content":"hi"}],"max_tokens":10}`))
	if err != nil {
		t.Fatal(err)
	}
	if ir2.System != "S1\n\nS2" {
		t.Fatalf("anthropic system 解析异常: %q", ir2.System)
	}
}

// TestNormalizeAPIType 类型规范化。
func TestNormalizeAPIType(t *testing.T) {
	cases := map[string]string{
		"":          FormatOpenAI,
		"openai":    FormatOpenAI,
		"OPENAI":    FormatOpenAI,
		"responses": FormatResponses,
		"anthropic": FormatAnthropic,
		"claude":    FormatAnthropic,
		"garbage":   FormatOpenAI,
	}
	for in, want := range cases {
		if got := NormalizeAPIType(in); got != want {
			t.Errorf("NormalizeAPIType(%q) = %q, want %q", in, got, want)
		}
	}
	if !ValidAPIType("") || !ValidAPIType("responses") || ValidAPIType("xxx") {
		t.Error("ValidAPIType 判断异常")
	}
}

// TestResponsesSSEEventSequence Responses SSE 事件序列完整性与顺序（对齐官方流式协议）。
// 回归覆盖：response.in_progress 缺失（Bug 1）、function_call_arguments.delta/done 缺失（Bug 2）。
func TestResponsesSSEEventSequence(t *testing.T) {
	ir := &ResponseIR{
		ID: "resp_1", Model: "m", Created: 1700000000, Text: "你好",
		ToolCalls: []ToolCall{{ID: "call_1", Name: "f", Args: `{"a":1}`}},
		Finish:    "tool_calls", PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12,
	}
	sse, err := BuildSSE(FormatResponses, BuildResponse(FormatResponses, ir, "m"))
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for _, line := range strings.Split(string(sse), "\n") {
		if e := strings.TrimPrefix(line, "event: "); e != line {
			events = append(events, e)
		}
	}
	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.output_item.added", // function_call 条目
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	}
	if len(events) != len(want) {
		t.Fatalf("事件数量 = %d, want %d\n实际序列: %v", len(events), len(want), events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("事件[%d] = %s, want %s\n实际序列: %v", i, events[i], want[i], events)
		}
	}
}

// TestResponsesSSEIncomplete finish=length 时应合成 response.incomplete 事件。
func TestResponsesSSEIncomplete(t *testing.T) {
	ir := &ResponseIR{
		ID: "r", Model: "m", Created: 1700000000, Text: "被截断", Finish: "length",
		PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3,
	}
	sse, err := BuildSSE(FormatResponses, BuildResponse(FormatResponses, ir, "m"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(sse)
	if !strings.Contains(s, "event: response.incomplete") {
		t.Error("finish=length 应发 response.incomplete 事件")
	}
	if !strings.Contains(s, "max_output_tokens") {
		t.Error("response.incomplete 应含 incomplete_details.reason=max_output_tokens")
	}
	if strings.Contains(s, "event: response.completed") {
		t.Error("finish=length 不应发 response.completed")
	}
}

// TestToolResultBlockArray Anthropic tool_result.content 为块数组时正确提取文本（BUG-A 回归）。
// 官方 SDK 确认 content 类型为 Union[str, Iterable[Content]]，数组形式是常用写法。
func TestToolResultBlockArray(t *testing.T) {
	// 1. 直接测试 rawJSONToText 各分支
	cases := []struct {
		tag  string
		raw  string
		want string
	}{
		{"字符串", `"hello"`, "hello"},
		{"单文本块", `[{"type":"text","text":"北京今天晴"}]`, "北京今天晴"},
		{"多文本块", `[{"type":"text","text":"A"},{"type":"text","text":"B"}]`, "A\nB"},
		{"混合块", `[{"type":"text","text":"结果"},{"type":"image","source":{"type":"base64","data":"..."}}]`, "结果"},
		{"纯图片块", `[{"type":"image","source":{"type":"url","url":"https://x.com/a.png"}}]`, ""},
		{"空数组", `[]`, ""},
		{"null", `null`, ""},
		{"裸JSON", `{"error":"timeout"}`, `{"error":"timeout"}`},
	}
	for _, c := range cases {
		got := rawJSONToText(json.RawMessage(c.raw))
		if got != c.want {
			t.Errorf("%s: rawJSONToText(%s) = %q, want %q", c.tag, c.raw, got, c.want)
		}
	}

	// 2. 完整 Anthropic 请求解析：tool_result content 为块数组
	req := []byte(`{"model":"m","max_tokens":10,
		"messages":[
			{"role":"user","content":"查天气"},
			{"role":"assistant","content":[
				{"type":"text","text":"好的"},
				{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{"city":"北京"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_01","content":[
					{"type":"text","text":"北京今天晴"},
					{"type":"text","text":"气温25度"}
				]}
			]}
		]}`)
	ir, err := ParseRequest(FormatAnthropic, req)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	var toolMsg *InputMessage
	for i := range ir.Messages {
		if ir.Messages[i].Role == "tool" {
			toolMsg = &ir.Messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("未找到 tool 消息")
	}
	if toolMsg.ToolCallID != "toolu_01" {
		t.Errorf("ToolCallID = %q, want toolu_01", toolMsg.ToolCallID)
	}
	want := "北京今天晴\n气温25度"
	if toolMsg.Text != want {
		t.Errorf("tool_result 块数组文本 = %q, want %q", toolMsg.Text, want)
	}
	if strings.HasPrefix(toolMsg.Text, "[") {
		t.Errorf("tool_result 文本不应是原始 JSON: %q", toolMsg.Text)
	}

	// 3. 跨格式验证：Anthropic tool_result 数组 → 转为 OpenAI/Responses → tool 消息文本正确
	for _, target := range []string{FormatOpenAI, FormatResponses} {
		body, err := BuildRequest(target, ir, "m")
		if err != nil {
			t.Fatalf("anthropic→%s build: %v", target, err)
		}
		back, err := ParseRequest(target, body)
		if err != nil {
			t.Fatalf("anthropic→%s parse: %v", target, err)
		}
		var tm *InputMessage
		for i := range back.Messages {
			if back.Messages[i].Role == "tool" {
				tm = &back.Messages[i]
			}
		}
		if tm == nil {
			t.Fatalf("anthropic→%s: 未找到 tool 消息", target)
		}
		if !strings.Contains(tm.Text, "北京今天晴") || !strings.Contains(tm.Text, "气温25度") {
			t.Errorf("anthropic→%s: tool 文本 = %q, 应包含块数组提取的文本", target, tm.Text)
		}
	}
}

// TestImageURLCompat image_url 非标准形式容错（Bug 5 回归测试）。
// Responses 官方为字符串形式，此处兼容对象形式 {"url":...,"detail":...}；
// CC 官方为对象形式，此处兼容纯字符串形式。
func TestImageURLCompat(t *testing.T) {
	rs := []byte(`{"model":"m","input":[{"role":"user","content":[
		{"type":"input_text","text":"图"},
		{"type":"input_image","image_url":{"url":"data:image/png;base64,QUJD","detail":"high"}}]}]}`)
	ir, err := ParseRequest(FormatResponses, rs)
	if err != nil {
		t.Fatalf("responses 对象形式 image_url 应解析成功: %v", err)
	}
	if len(ir.Messages[0].Parts) != 2 || ir.Messages[0].Parts[1].ImageB64 != "QUJD" {
		t.Fatalf("responses 对象形式 image_url 图片丢失: %+v", ir.Messages[0].Parts)
	}

	cc := []byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}]}`)
	ir2, err := ParseRequest(FormatOpenAI, cc)
	if err != nil {
		t.Fatalf("openai 字符串形式 image_url 应解析成功: %v", err)
	}
	if len(ir2.Messages[0].Parts) != 1 || ir2.Messages[0].Parts[0].ImageB64 != "QUJD" {
		t.Fatalf("openai 字符串形式 image_url 图片丢失: %+v", ir2.Messages[0].Parts)
	}
}

// TestAssistantToolCallTextOrder 同一 assistant 回合中"工具调用在前、文本在后"
// （Codex 断流重试时回放历史的真实序列）不得拆分为两条 assistant 消息：
// 拆分后 tool 结果消息不再紧跟发起调用的 assistant，违反 Chat Completions
// 语义，校验严格的上游（如 MiniMax）直接 400 invalid params（v1.7.3 回归测试）。
func TestAssistantToolCallTextOrder(t *testing.T) {
	// Codex 真实回放序列：user → function_call → function_call_output →
	// function_call → assistant 文本(preamble) → function_call_output
	rs := []byte(`{"model":"m","stream":true,"instructions":"sys",
	"input":[
		{"role":"user","content":[{"type":"input_text","text":"帮我查天气"}]},
		{"type":"function_call","call_id":"call_A","name":"get_weather","arguments":"{\"city\":\"上海\"}"},
		{"type":"function_call_output","call_id":"call_A","output":"{\"temp\":25}"},
		{"type":"function_call","call_id":"call_B","name":"get_weather","arguments":"{\"city\":\"北京\"}"},
		{"role":"assistant","content":[{"type":"output_text","text":"我先查一下两个城市。"}]},
		{"type":"function_call_output","call_id":"call_B","output":"{\"temp\":20}"}
	]}`)
	ir, err := ParseRequest(FormatResponses, rs)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	t.Run("IR合并", func(t *testing.T) {
		// 期望 4 条：user / assistant(tcA) / tool(A) / assistant(tcB+text) / tool(B) —— 共 5 条
		if len(ir.Messages) != 5 {
			t.Fatalf("消息数 = %d, want 5:\n%+v", len(ir.Messages), ir.Messages)
		}
		last := ir.Messages[3] // 第二条 assistant：tool_calls 与文本合并
		if last.Role != "assistant" || last.Text != "我先查一下两个城市。" || len(last.ToolCalls) != 1 || last.ToolCalls[0].ID != "call_B" {
			t.Fatalf("文本未合并进带 tool_calls 的 assistant: %+v", ir.Messages[3])
		}
	})

	t.Run("OpenAI顺序合法", func(t *testing.T) {
		body, err := BuildRequest(FormatOpenAI, ir, "real")
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		var m struct {
			Messages []json.RawMessage `json:"messages"`
		}
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		var roles []string
		for _, raw := range m.Messages {
			var msg struct {
				Role      string `json:"role"`
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			}
			json.Unmarshal(raw, &msg)
			role := msg.Role
			if len(msg.ToolCalls) > 0 {
				role += "*"
			}
			roles = append(roles, role)
		}
		// 期望：system user assistant* tool assistant* tool（文本与 call_B 同一条 assistant）
		want := "system,user,assistant*,tool,assistant*,tool"
		if got := strings.Join(roles, ","); got != want {
			t.Fatalf("消息序列 = %s, want %s", got, want)
		}
		// 合并的 assistant 消息须同时含 content（字符串或单 text 块数组）与 tool_calls
		var merged struct {
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		}
		json.Unmarshal(m.Messages[4], &merged)
		if !strings.Contains(string(merged.Content), "我先查一下两个城市。") {
			t.Fatalf("合并的 assistant content 丢失文本: %s", merged.Content)
		}
		if len(merged.ToolCalls) != 1 || merged.ToolCalls[0].ID != "call_B" {
			t.Fatalf("合并的 assistant tool_calls 错误: %s", m.Messages[4])
		}
	})

	t.Run("Anthropic顺序合法", func(t *testing.T) {
		body, err := BuildRequest(FormatAnthropic, ir, "real")
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		var m struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// tool_result（user）必须紧跟含 tool_use 的 assistant
		var roles []string
		for _, msg := range m.Messages {
			roles = append(roles, msg.Role)
		}
		if got := strings.Join(roles, ","); got != "user,assistant,user,assistant,user" {
			t.Fatalf("anthropic 消息序列 = %s, want user,assistant,user,assistant,user", got)
		}
	})

	t.Run("反向顺序同样合并", func(t *testing.T) {
		// 文本在前、工具调用在后（常规顺序）也必须合并为同一条 assistant
		rs2 := []byte(`{"model":"m","input":[
			{"role":"user","content":"查天气"},
			{"role":"assistant","content":[{"type":"output_text","text":"好的。"}]},
			{"type":"function_call","call_id":"call_C","name":"get_weather","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_C","output":"ok"}
		]}`)
		ir2, err := ParseRequest(FormatResponses, rs2)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(ir2.Messages) != 3 {
			t.Fatalf("消息数 = %d, want 3（user/assistant(text+tcC)/tool）:\n%+v", len(ir2.Messages), ir2.Messages)
		}
		a := ir2.Messages[1]
		if a.Text != "好的。" || len(a.ToolCalls) != 1 || a.ToolCalls[0].ID != "call_C" {
			t.Fatalf("文本与工具调用未合并: %+v", a)
		}
	})
}
