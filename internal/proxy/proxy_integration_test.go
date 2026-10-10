// proxy_integration_test.go：跨格式代理转发的集成测试。
// 使用 httptest 模拟三种格式的上游供应商，验证 3 客户端格式 × 3 供应商类型的全部组合、
// 流式合成、工具调用转换、认证兼容（x-api-key）、错误格式与 /v1/models。
package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"llmproxy/internal/config"
	"llmproxy/internal/convert"
	"llmproxy/internal/core"
	"llmproxy/internal/logsdb"
)

const upstreamKey = "sk-upstream-test-key"

// startMockUpstream 启动同时支持三种格式的 mock 上游。
// 响应正文带 [格式] 标记与请求摘要（sys 是否收到 / 消息数 / 末条用户文本），供端到端断言。
func startMockUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	marker := func(format string, hasSystem bool, msgCount int, lastUser, model string) string {
		return fmt.Sprintf("[%s] sys=%v msgs=%d last=%s model=%s", format, hasSystem, msgCount, lastUser, model)
	}
	// extractText 从 content（字符串或片段数组）提取文本
	extractText := func(content any) string {
		switch c := content.(type) {
		case string:
			return c
		case []any:
			var sb strings.Builder
			for _, p := range c {
				if m, ok := p.(map[string]any); ok {
					if t, ok := m["text"].(string); ok {
						sb.WriteString(t)
					}
				}
			}
			return sb.String()
		}
		return ""
	}
	mux := http.NewServeMux()

	fail401 := func(w http.ResponseWriter, format string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		if format == "anthropic" {
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
		} else {
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"invalid_request_error","code":"invalid_api_key"}}`))
		}
	}

	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+upstreamKey {
			fail401(w, "openai")
			return
		}
		var req struct {
			Model    string           `json:"model"`
			Messages []map[string]any `json:"messages"`
			Stream   bool             `json:"stream"`
			Tools    []any            `json:"tools"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		hasSystem, lastUser := false, ""
		for _, m := range req.Messages {
			if m["role"] == "system" || m["role"] == "developer" {
				hasSystem = true
			}
			if m["role"] == "user" {
				lastUser = extractText(m["content"])
			}
		}
		text := marker("openai", hasSystem, len(req.Messages), lastUser, req.Model)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(lastUser, "工具") && len(req.Tools) > 0 {
			_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"chatcmpl-mock","object":"chat.completion","created":1700000000,"model":%q,
				"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_mock_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"上海\"}"}}]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`, req.Model)))
			return
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, canFlush := w.(http.Flusher)
			time.Sleep(60 * time.Millisecond) // 模拟首 token 延迟（TTFT）
			fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-mock\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n", req.Model)
			if canFlush {
				flusher.Flush()
			}
			time.Sleep(40 * time.Millisecond) // 生成中：分帧输出
			fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-mock\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", req.Model, text)
			if canFlush {
				flusher.Flush()
			}
			time.Sleep(40 * time.Millisecond) // 生成结束：finish + usage
			fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-mock\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":22,\"total_tokens\":33}}\n\n", req.Model)
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"chatcmpl-mock","object":"chat.completion","created":1700000000,"model":%q,
			"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`, req.Model, text)))
	})

	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+upstreamKey {
			fail401(w, "responses")
			return
		}
		var req struct {
			Model        string          `json:"model"`
			Input        json.RawMessage `json:"input"`
			Instructions string          `json:"instructions"`
			Stream       bool            `json:"stream"`
			Tools        []any           `json:"tools"`
			MaxOutputTok int             `json:"max_output_tokens"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		// input 可能是字符串或条目数组
		var items []map[string]any
		var s string
		if err := json.Unmarshal(req.Input, &s); err == nil {
			items = []map[string]any{{"role": "user", "content": s}}
		} else {
			_ = json.Unmarshal(req.Input, &items)
		}
		hasSystem := req.Instructions != ""
		lastUser := ""
		for _, it := range items {
			if it["role"] == "system" || it["role"] == "developer" {
				hasSystem = true
			}
			if it["role"] == "user" {
				lastUser = extractText(it["content"])
			}
		}
		text := marker("responses", hasSystem, len(items), lastUser, req.Model)
		w.Header().Set("Content-Type", "application/json")
		if req.Stream {
			// 流式：返回 Responses 格式 SSE 事件流（分帧 + 延迟，供实时转发验证）
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			frame := func(event string, obj any) {
				b, _ := json.Marshal(obj)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
				if flusher != nil {
					flusher.Flush()
				}
			}
			runes := []rune(text)
			half := len(runes) / 2
			part1, part2 := string(runes[:half]), string(runes[half:])
			respObj := func(status string) map[string]any {
				return map[string]any{"id": "resp_mock", "object": "response", "created_at": 1700000000,
					"status": status, "model": req.Model, "output": []any{}}
			}
			time.Sleep(50 * time.Millisecond) // TTFT
			frame("response.created", map[string]any{"type": "response.created", "response": respObj("in_progress")})
			frame("response.in_progress", map[string]any{"type": "response.in_progress", "response": respObj("in_progress")})
			frame("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0,
				"item": map[string]any{"type": "message", "id": "msg_mock", "status": "in_progress", "role": "assistant", "content": []any{}}})
			frame("response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": "msg_mock",
				"output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
			time.Sleep(40 * time.Millisecond)
			frame("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg_mock",
				"output_index": 0, "content_index": 0, "delta": part1})
			time.Sleep(40 * time.Millisecond)
			frame("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg_mock",
				"output_index": 0, "content_index": 0, "delta": part2})
			msgItem := map[string]any{"type": "message", "id": "msg_mock", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
			frame("response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": "msg_mock",
				"output_index": 0, "content_index": 0, "text": text})
			frame("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": msgItem})
			time.Sleep(40 * time.Millisecond)
			frame("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{
				"id": "resp_mock", "object": "response", "created_at": 1700000000, "status": "completed",
				"model": req.Model, "output": []any{msgItem},
				"usage":       map[string]int64{"input_tokens": 44, "output_tokens": 55, "total_tokens": 99},
				"output_text": text}})
			return
		}
		if strings.Contains(lastUser, "工具") && len(req.Tools) > 0 {
			_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"resp_mock","object":"response","created_at":1700000000,"status":"completed","model":%q,
				"output":[{"type":"function_call","id":"fc_mock","call_id":"call_mock_1","name":"get_weather","arguments":"{\"city\":\"上海\"}","status":"completed"}],
				"usage":{"input_tokens":44,"output_tokens":55,"total_tokens":99}}`, req.Model)))
			return
		}
		_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"resp_mock","object":"response","created_at":1700000000,"status":"completed","model":%q,
			"output":[{"type":"message","id":"msg_mock","status":"completed","role":"assistant","content":[{"type":"output_text","text":%q,"annotations":[]}]}],
			"usage":{"input_tokens":44,"output_tokens":55,"total_tokens":99}}`, req.Model, text)))
	})

	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != upstreamKey {
			fail401(w, "anthropic")
			return
		}
		if r.Header.Get("Anthropic-Version") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"missing anthropic-version"}}`))
			return
		}
		var req struct {
			Model    string           `json:"model"`
			System   string           `json:"system"`
			Messages []map[string]any `json:"messages"`
			Stream   bool             `json:"stream"`
			Tools    []any            `json:"tools"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		lastUser := ""
		for _, m := range req.Messages {
			if m["role"] == "user" {
				lastUser = extractText(m["content"])
			}
		}
		text := marker("anthropic", req.System != "", len(req.Messages), lastUser, req.Model)
		w.Header().Set("Content-Type", "application/json")
		if req.Stream {
			// 流式：返回 Anthropic 格式 SSE 事件流（分帧 + 延迟，供实时转发验证）
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			frame := func(event string, obj any) {
				b, _ := json.Marshal(obj)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
				if flusher != nil {
					flusher.Flush()
				}
			}
			runes := []rune(text)
			half := len(runes) / 2
			part1, part2 := string(runes[:half]), string(runes[half:])
			time.Sleep(50 * time.Millisecond) // TTFT
			frame("message_start", map[string]any{"type": "message_start", "message": map[string]any{
				"id": "msg_mock", "type": "message", "role": "assistant", "model": req.Model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]int64{"input_tokens": 66, "output_tokens": 1}}})
			frame("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
				"content_block": map[string]any{"type": "text", "text": ""}})
			time.Sleep(40 * time.Millisecond)
			frame("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": part1}})
			time.Sleep(40 * time.Millisecond)
			frame("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": part2}})
			frame("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
			time.Sleep(40 * time.Millisecond)
			frame("message_delta", map[string]any{"type": "message_delta",
				"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
				"usage": map[string]int64{"output_tokens": 77}})
			frame("message_stop", map[string]any{"type": "message_stop"})
			return
		}
		if strings.Contains(lastUser, "工具") && len(req.Tools) > 0 {
			_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"msg_mock","type":"message","role":"assistant","model":%q,
				"content":[{"type":"tool_use","id":"toolu_mock_1","name":"get_weather","input":{"city":"上海"}}],
				"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":66,"output_tokens":77}}`, req.Model)))
			return
		}
		_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"msg_mock","type":"message","role":"assistant","model":%q,
			"content":[{"type":"text","text":%q}],"stop_reason":"end_turn","stop_sequence":null,
			"usage":{"input_tokens":66,"output_tokens":77}}`, req.Model, text)))
	})

	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+upstreamKey && r.Header.Get("X-Api-Key") != upstreamKey {
			fail401(w, "openai")
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"mock-model-a"},{"id":"mock-model-b"}]}`))
	})

	return httptest.NewServer(mux)
}

// newTestStack 构建带三个不同类型供应商的代理处理器。
func newTestStack(t *testing.T, upstreamURL string) (*Handler, *core.State, *config.Manager, func()) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	mgr := config.NewManager(cfgPath)
	if _, err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	state := core.NewState(mgr)
	err := state.Mutate(func(ns *core.Snapshot) {
		mkProvider := func(name, apiType, model, alias string) *config.Provider {
			return &config.Provider{
				ID: name, Name: name, BaseURL: upstreamURL + "/v1", APIType: apiType,
				Enabled: true, Weight: 1,
				APIKeys: []config.ProviderAPIKey{{ID: "k-" + name, Key: upstreamKey}},
				Models: []config.ProviderModel{{Name: model, Aliases: func() []string {
					if alias != "" {
						return []string{alias}
					}
					return nil
				}()}},
			}
		}
		// anthropic 供应商的 base 不带 /v1（测试 /v1/messages 智能拼接）
		pAnt := mkProvider("p-openai", convert.FormatOpenAI, "m-openai", "alias-oi")
		pRes := mkProvider("p-responses", convert.FormatResponses, "m-responses", "")
		pAnt2 := mkProvider("p-anthropic", convert.FormatAnthropic, "m-anthropic", "")
		pAnt2.BaseURL = upstreamURL
		ns.Providers = []*config.Provider{pAnt, pRes, pAnt2}
		ns.APIKeys = []*config.ProxyKey{{
			ID: "pk1", Name: "e2e", Key: "sk-proxy-e2e-key", Enabled: true,
			IPWhitelist: []string{"*"}, Models: []string{},
		}}
		ns.LogEnabled = true // 测试栈显式开启日志（v1.8.1 起默认关闭）
	})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(state)
	cleanup := func() { state.Stop() }
	return h, state, mgr, cleanup
}

// postProxy 以指定格式请求代理端点。
func postProxy(t *testing.T, h *Handler, clientFormat, model, body string, header map[string]string) (int, string, http.Header) {
	t.Helper()
	endpoints := map[string]string{
		convert.FormatOpenAI:    "/v1/chat/completions",
		convert.FormatResponses: "/v1/responses",
		convert.FormatAnthropic: "/v1/messages",
	}
	// 允许 body 中使用 %s 占位模型名
	body = strings.ReplaceAll(body, "@MODEL@", model)
	req := httptest.NewRequest(http.MethodPost, endpoints[clientFormat], strings.NewReader(body))
	if header == nil {
		req.Header.Set("Authorization", "Bearer sk-proxy-e2e-key")
	} else {
		for k, v := range header {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeV1(rec, req)
	resp := rec.Result()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data), resp.Header
}

// TestAllNineCombinations 3 客户端格式 × 3 供应商类型 = 9 种组合。
func TestAllNineCombinations(t *testing.T) {
	up := startMockUpstream(t)
	defer up.Close()
	h, _, _, cleanup := newTestStack(t, up.URL)
	defer cleanup()

	clientBodies := map[string]string{
		convert.FormatOpenAI:    `{"model":"@MODEL@","messages":[{"role":"system","content":"你是测试助手"},{"role":"user","content":"你好"}],"max_tokens":50}`,
		convert.FormatResponses: `{"model":"@MODEL@","input":[{"role":"system","content":"你是测试助手"},{"role":"user","content":"你好"}],"max_output_tokens":50}`,
		convert.FormatAnthropic: `{"model":"@MODEL@","system":"你是测试助手","messages":[{"role":"user","content":"你好"}],"max_tokens":50}`,
	}
	wantUsage := map[string][3]int64{
		"m-openai":    {11, 22, 33},
		"m-responses": {44, 55, 99},
		"m-anthropic": {66, 77, 143},
	}

	for _, clientFmt := range convert.APITypes() {
		for _, model := range []string{"m-openai", "m-responses", "m-anthropic"} {
			tag := clientFmt + "→" + model
			status, body, _ := postProxy(t, h, clientFmt, model, clientBodies[clientFmt], nil)
			if status != 200 {
				t.Errorf("[%s] status=%d body=%s", tag, status, body)
				continue
			}
			// 期望正文包含上游格式标记（证明请求被转发到了正确类型的供应商）
			var upFmt string
			switch model {
			case "m-openai":
				upFmt = "[openai]"
			case "m-responses":
				upFmt = "[responses]"
			case "m-anthropic":
				upFmt = "[anthropic]"
			}
			if !strings.Contains(body, upFmt) {
				t.Errorf("[%s] 响应缺少上游标记 %s: %s", tag, upFmt, body)
			}
			// 系统词透传验证
			if !strings.Contains(body, "sys=true") {
				t.Errorf("[%s] 系统提示词未透传: %s", tag, body)
			}
			if !strings.Contains(body, "last=你好") {
				t.Errorf("[%s] 用户消息未透传: %s", tag, body)
			}
			// usage 映射验证（按客户端格式的字段名）
			u := wantUsage[model]
			var obj map[string]json.RawMessage
			if err := json.Unmarshal([]byte(body), &obj); err != nil {
				t.Errorf("[%s] 响应非 JSON: %s", tag, body)
				continue
			}
			var usageStr string
			_ = json.Unmarshal(obj["usage"], &usageStr)
			var want string
			switch clientFmt {
			case convert.FormatOpenAI:
				want = fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}`, u[0], u[1], u[2])
			case convert.FormatResponses:
				want = fmt.Sprintf(`{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d}`, u[0], u[1], u[2])
			case convert.FormatAnthropic:
				want = fmt.Sprintf(`{"input_tokens":%d,"output_tokens":%d}`, u[0], u[1])
			}
			got := string(obj["usage"])
			// JSON 字段顺序可能与构造顺序不同，逐一解析比较
			var gotU map[string]int64
			_ = json.Unmarshal(obj["usage"], &gotU)
			var wantU map[string]int64
			_ = json.Unmarshal([]byte(want), &wantU)
			if fmt.Sprint(gotU) != fmt.Sprint(wantU) {
				t.Errorf("[%s] usage 异常: got %s want %s (%s vs %s)", tag, got, want, usageStr, "")
			}
			// 客户端格式结构验证
			switch clientFmt {
			case convert.FormatOpenAI:
				if !strings.Contains(body, `"object":"chat.completion"`) || !strings.Contains(body, `"finish_reason":"stop"`) {
					t.Errorf("[%s] CC 响应结构异常: %s", tag, body)
				}
			case convert.FormatResponses:
				if !strings.Contains(body, `"object":"response"`) || !strings.Contains(body, `"status":"completed"`) {
					t.Errorf("[%s] Responses 响应结构异常: %s", tag, body)
				}
			case convert.FormatAnthropic:
				if !strings.Contains(body, `"type":"message"`) || !strings.Contains(body, `"stop_reason":"end_turn"`) {
					t.Errorf("[%s] Anthropic 响应结构异常: %s", tag, body)
				}
			}
		}
	}
}

// TestCrossFormatStreaming 跨格式流式：客户端 stream=true 时返回对应格式 SSE。
func TestCrossFormatStreaming(t *testing.T) {
	up := startMockUpstream(t)
	defer up.Close()
	h, _, _, cleanup := newTestStack(t, up.URL)
	defer cleanup()

	cases := []struct {
		clientFmt, model string
		check            func(t *testing.T, sse string)
	}{
		{convert.FormatOpenAI, "m-responses", func(t *testing.T, sse string) {
			if !strings.Contains(sse, "data: ") || !strings.Contains(sse, "[DONE]") {
				t.Errorf("CC SSE 异常: %s", sse)
			}
			if !strings.Contains(sse, "[responses]") {
				t.Errorf("CC SSE 缺少上游标记: %s", sse)
			}
		}},
		{convert.FormatAnthropic, "m-openai", func(t *testing.T, sse string) {
			for _, ev := range []string{"event: message_start", "event: content_block_delta", "event: message_delta", "event: message_stop"} {
				if !strings.Contains(sse, ev) {
					t.Errorf("Anthropic SSE 缺少 %s: %s", ev, sse)
				}
			}
			if !strings.Contains(sse, "[openai]") {
				t.Errorf("Anthropic SSE 缺少上游标记: %s", sse)
			}
		}},
		{convert.FormatResponses, "m-anthropic", func(t *testing.T, sse string) {
			for _, ev := range []string{"event: response.created", "event: response.output_text.delta", "event: response.completed"} {
				if !strings.Contains(sse, ev) {
					t.Errorf("Responses SSE 缺少 %s: %s", ev, sse)
				}
			}
			if !strings.Contains(sse, "[anthropic]") {
				t.Errorf("Responses SSE 缺少上游标记: %s", sse)
			}
		}},
	}
	for _, c := range cases {
		body := map[string]string{
			convert.FormatOpenAI:    `{"model":"@MODEL@","messages":[{"role":"user","content":"流式测试"}],"stream":true}`,
			convert.FormatResponses: `{"model":"@MODEL@","input":"流式测试","stream":true}`,
			convert.FormatAnthropic: `{"model":"@MODEL@","messages":[{"role":"user","content":"流式测试"}],"max_tokens":50,"stream":true}`,
		}[c.clientFmt]
		status, respBody, hdr := postProxy(t, h, c.clientFmt, c.model, body, nil)
		if status != 200 {
			t.Fatalf("%s→%s stream status=%d body=%s", c.clientFmt, c.model, status, respBody)
		}
		if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
			t.Errorf("%s→%s Content-Type = %q, want text/event-stream", c.clientFmt, c.model, ct)
		}
		c.check(t, respBody)
		// usage 可从合成 SSE 提取
		p, cc, _ := convert.ExtractUsage([]byte(respBody), true)
		if p == 0 && cc == 0 {
			t.Errorf("%s→%s 合成 SSE 无法提取 usage", c.clientFmt, c.model)
		}
	}
}

// TestSameFormatStreamPassthrough 同格式流式仍为透传（上游 SSE 原样返回）。
func TestSameFormatStreamPassthrough(t *testing.T) {
	up := startMockUpstream(t)
	defer up.Close()
	h, _, _, cleanup := newTestStack(t, up.URL)
	defer cleanup()

	status, body, hdr := postProxy(t, h, convert.FormatOpenAI, "m-openai",
		`{"model":"@MODEL@","messages":[{"role":"user","content":"流式测试"}],"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if !strings.Contains(hdr.Get("Content-Type"), "text/event-stream") {
		t.Errorf("Content-Type = %q", hdr.Get("Content-Type"))
	}
	if !strings.Contains(body, "data: [DONE]") || !strings.Contains(body, "[openai]") {
		t.Errorf("同格式流式透传异常: %s", body)
	}
}

// TestToolCallConversion 跨格式工具调用转换。
func TestToolCallConversion(t *testing.T) {
	up := startMockUpstream(t)
	defer up.Close()
	h, _, _, cleanup := newTestStack(t, up.URL)
	defer cleanup()

	ccTool := `{"model":"@MODEL@","messages":[{"role":"user","content":"请用工具查询"}],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"查询天气","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`

	// CC 客户端 → anthropic 供应商
	status, body, _ := postProxy(t, h, convert.FormatOpenAI, "m-anthropic", ccTool, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if !strings.Contains(body, `"tool_calls"`) || !strings.Contains(body, `"get_weather"`) ||
		!strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("CC←Anthropic 工具调用转换异常: %s", body)
	}

	// Anthropic 客户端 → responses 供应商
	antTool := `{"model":"@MODEL@","messages":[{"role":"user","content":"请用工具查询"}],"max_tokens":100,
		"tools":[{"name":"get_weather","description":"查询天气","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
	status, body, _ = postProxy(t, h, convert.FormatAnthropic, "m-responses", antTool, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if !strings.Contains(body, `"tool_use"`) || !strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Errorf("Anthropic←Responses 工具调用转换异常: %s", body)
	}

	// Responses 客户端 → openai 供应商
	respTool := `{"model":"@MODEL@","input":[{"role":"user","content":"请用工具查询"}],
		"tools":[{"type":"function","name":"get_weather","description":"查询天气","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
	status, body, _ = postProxy(t, h, convert.FormatResponses, "m-openai", respTool, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if !strings.Contains(body, `"function_call"`) || !strings.Contains(body, `"get_weather"`) {
		t.Errorf("Responses←OpenAI 工具调用转换异常: %s", body)
	}
}

// TestAuthAndErrors 认证兼容与错误格式。
func TestAuthAndErrors(t *testing.T) {
	up := startMockUpstream(t)
	defer up.Close()
	h, _, _, cleanup := newTestStack(t, up.URL)
	defer cleanup()

	// x-api-key 认证（Anthropic SDK 风格）调用 /v1/messages → openai 供应商
	status, body, _ := postProxy(t, h, convert.FormatAnthropic, "m-openai",
		`{"model":"@MODEL@","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`,
		map[string]string{"X-Api-Key": "sk-proxy-e2e-key"})
	if status != 200 || !strings.Contains(body, "[openai]") {
		t.Errorf("x-api-key 认证失败: status=%d body=%s", status, body)
	}

	// 错误 Key → /v1/messages 返回 Anthropic 错误格式
	status, body, _ = postProxy(t, h, convert.FormatAnthropic, "m-openai",
		`{"model":"@MODEL@","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`,
		map[string]string{"X-Api-Key": "sk-wrong"})
	if status != 401 || !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "authentication_error") {
		t.Errorf("Anthropic 401 格式异常: status=%d body=%s", status, body)
	}

	// 错误 Key → /v1/chat/completions 返回 OpenAI 错误格式
	status, body, _ = postProxy(t, h, convert.FormatOpenAI, "m-openai",
		`{"model":"@MODEL@","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer sk-wrong"})
	if status != 401 || !strings.Contains(body, `"error"`) || !strings.Contains(body, "invalid_api_key") {
		t.Errorf("OpenAI 401 格式异常: status=%d body=%s", status, body)
	}

	// 未知模型 → /v1/messages 404 Anthropic 格式
	status, body, _ = postProxy(t, h, convert.FormatAnthropic, "m-not-exist",
		`{"model":"@MODEL@","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`, nil)
	if status != 404 || !strings.Contains(body, "not_found_error") {
		t.Errorf("Anthropic 404 格式异常: status=%d body=%s", status, body)
	}

	// 未知模型 → /v1/responses 404 OpenAI 格式
	status, body, _ = postProxy(t, h, convert.FormatResponses, "m-not-exist",
		`{"model":"@MODEL@","input":"hi"}`, nil)
	if status != 404 || !strings.Contains(body, "model_not_found") {
		t.Errorf("Responses 404 格式异常: status=%d body=%s", status, body)
	}

	// 缺少 model → 400
	status, body, _ = postProxy(t, h, convert.FormatResponses, "", `{"input":"hi"}`, nil)
	if status != 400 {
		t.Errorf("缺少 model 应 400: status=%d body=%s", status, body)
	}

	// /v1/responses 收到非法格式 body → 400
	status, body, _ = postProxy(t, h, convert.FormatResponses, "m-openai", `{"model":"@MODEL@","input":{"bad":[1,2]}}`, nil)
	if status != 400 {
		t.Errorf("非法 input 应 400: status=%d body=%s", status, body)
	}
}

// TestModelsEndpointSuperset /v1/models 返回 OpenAI+Anthropic 超集格式。
func TestModelsEndpointSuperset(t *testing.T) {
	up := startMockUpstream(t)
	defer up.Close()
	h, _, _, cleanup := newTestStack(t, up.URL)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-proxy-e2e-key")
	rec := httptest.NewRecorder()
	h.ServeV1(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp struct {
		Data []struct {
			ID          string `json:"id"`
			Type        string `json:"type"`
			DisplayName string `json:"display_name"`
			AliasFor    string `json:"alias_for"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	foundModel, foundAlias := false, false
	for _, m := range resp.Data {
		if m.ID == "m-openai" {
			foundModel = true
			if m.Type != "model" || m.DisplayName == "" {
				t.Errorf("模型条目缺少 Anthropic 兼容字段: %+v", m)
			}
		}
		if m.ID == "alias-oi" && m.AliasFor == "m-openai" {
			foundAlias = true
		}
	}
	if !foundModel || !foundAlias {
		t.Errorf("模型列表缺失: %+v", resp.Data)
	}
}

// TestAliasSameFormatPassthrough 同格式别名命中时仅替换 model 字段。
func TestAliasSameFormatPassthrough(t *testing.T) {
	up := startMockUpstream(t)
	defer up.Close()
	h, _, _, cleanup := newTestStack(t, up.URL)
	defer cleanup()

	// 用别名 alias-oi 调用（openai→openai），上游应收到的 model 为真实名 m-openai
	status, body, _ := postProxy(t, h, convert.FormatOpenAI, "alias-oi",
		`{"model":"@MODEL@","messages":[{"role":"user","content":"你好"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if !strings.Contains(body, "model=m-openai") {
		t.Errorf("别名未替换为真实模型名: %s", body)
	}
	// 大小写不敏感命中
	status, body, _ = postProxy(t, h, convert.FormatOpenAI, "M-OPENAI",
		`{"model":"@MODEL@","messages":[{"role":"user","content":"你好"}]}`, nil)
	if status != 200 || !strings.Contains(body, "model=m-openai") {
		t.Errorf("大小写命中未替换: status=%d body=%s", status, body)
	}
	// 跨格式调用别名：CC 客户端 → responses 供应商（别名替换 + 格式转换同时生效）
	// 给 m-responses 加别名的场景已由 9 组合覆盖；此处验证 openai 供应商别名跨格式
	status, body, _ = postProxy(t, h, convert.FormatAnthropic, "alias-oi",
		`{"model":"@MODEL@","messages":[{"role":"user","content":"你好"}],"max_tokens":10}`, nil)
	if status != 200 || !strings.Contains(body, "model=m-openai") {
		t.Errorf("跨格式别名替换异常: status=%d body=%s", status, body)
	}
}

// TestLogWrittenWithUsage 验证日志写入（含 usage 归一化）。
func TestLogWrittenWithUsage(t *testing.T) {
	up := startMockUpstream(t)
	defer up.Close()
	dir := t.TempDir()
	store, err := logsdb.Open(filepath.Join(dir, "test-log.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// 重新构建 stack 以注入日志存储
	mgr := config.NewManager(filepath.Join(dir, "cfg.yml"))
	if _, err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	state := core.NewState(mgr)
	defer state.Stop()
	err = state.Mutate(func(ns *core.Snapshot) {
		ns.Providers = []*config.Provider{{
			ID: "p1", Name: "p-anthropic", BaseURL: up.URL, APIType: convert.FormatAnthropic,
			Enabled: true, Weight: 1,
			APIKeys: []config.ProviderAPIKey{{ID: "k1", Key: upstreamKey}},
			Models:  []config.ProviderModel{{Name: "m-anthropic"}},
		}}
		ns.APIKeys = []*config.ProxyKey{{ID: "pk1", Name: "e2e", Key: "sk-proxy-e2e-key", Enabled: true, IPWhitelist: []string{"*"}}}
		ns.LogEnabled = true // 测试栈显式开启日志（v1.8.1 起默认关闭）
	})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(state)
	h.SetLogStore(store)

	// CC 客户端 → anthropic 供应商（跨格式，usage 66/77/143）
	status, body, _ := postProxy(t, h, convert.FormatOpenAI, "m-anthropic",
		`{"model":"@MODEL@","messages":[{"role":"user","content":"你好"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	// 等待异步日志落库
	var logs *logsdb.LogPage
	for i := 0; i < 50; i++ {
		logs, err = store.Query(logsdb.LogQuery{Page: 1, PageSize: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(logs.Logs) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(logs.Logs) == 0 {
		t.Fatal("日志未写入")
	}
	l := logs.Logs[0]
	if l.Status != "success" || l.Provider != "p-anthropic" {
		t.Errorf("日志状态异常: %+v", l)
	}
	if l.PromptTokens != 66 || l.CompletionTokens != 77 || l.TotalTokens != 143 {
		t.Errorf("usage 归一化异常: %d/%d/%d", l.PromptTokens, l.CompletionTokens, l.TotalTokens)
	}
	// token 速率 = completion_tokens / 生成耗时秒（跨格式非流式：生成耗时=总耗时）
	// 耗时不足 1ms 记 0 时速率亦记 0
	if l.DurationMs < 0 {
		t.Fatalf("日志耗时异常: %d", l.DurationMs)
	}
	if l.GenerationMs != l.DurationMs {
		t.Errorf("非流式生成耗时应等于总耗时: gen=%d dur=%d", l.GenerationMs, l.DurationMs)
	}
	if l.DurationMs == 0 {
		if l.TokensPerSecond != 0 {
			t.Errorf("耗时 0ms 时速率应为 0，实际 %v", l.TokensPerSecond)
		}
	} else {
		wantRate := 77.0 / (float64(l.GenerationMs) / 1000.0)
		if l.TokensPerSecond <= 0 || math.Abs(l.TokensPerSecond-wantRate) > 1e-6 {
			t.Errorf("输出速率异常: got %v, want %v (77 tokens / %d ms)", l.TokensPerSecond, wantRate, l.GenerationMs)
		}
	}
	// 详情含四组数据：上游请求（URL/头/体）、上游响应、转换后发给客户端的响应
	detail, err := store.Get(l.ID)
	if err != nil || detail == nil {
		t.Fatalf("日志详情查询失败: %v", err)
	}
	if !strings.Contains(detail.UpstreamURL, "/v1/messages") {
		t.Errorf("上游 URL 记录异常: %s", detail.UpstreamURL)
	}
	// 上游请求体（跨格式转换后的 anthropic 请求）与认证头脱敏
	if !strings.Contains(detail.UpstreamReqBody, "\"max_tokens\"") {
		t.Errorf("上游请求体应记录转换后的 anthropic 请求: %s", detail.UpstreamReqBody)
	}
	if detail.UpstreamReqHeaders == "" || !strings.Contains(detail.UpstreamReqHeaders, "X-Api-Key") {
		t.Errorf("上游请求头应记录: %s", detail.UpstreamReqHeaders)
	}
	if strings.Contains(detail.UpstreamReqHeaders, upstreamKey) {
		t.Errorf("上游请求头认证字段应脱敏: %s", detail.UpstreamReqHeaders)
	}
	// resp_body 统一为上游响应体副本
	if !strings.Contains(detail.RespBody, "msg_mock") {
		t.Errorf("resp_body 应记录上游原始响应: %s", detail.RespBody)
	}
	// client_resp_body 为转换后发给客户端的响应
	if !strings.Contains(detail.ClientRespBody, "chat.completion") {
		t.Errorf("client_resp_body 应记录返回给客户端的转换后响应: %s", detail.ClientRespBody)
	}
	if detail.ClientRespStatus != 200 {
		t.Errorf("client_resp_status 应为 200: %d", detail.ClientRespStatus)
	}
	if detail.ClientRespHeaders == "" || !strings.Contains(detail.ClientRespHeaders, "Content-Type") {
		t.Errorf("client_resp_headers 应记录: %s", detail.ClientRespHeaders)
	}
}

// TestStreamingGenerationRate 流式调用的生成耗时与输出速率：
// 生成耗时 = 首 chunk（首 token）→ 流结束，应明显小于总耗时（总耗时含首 token 延迟）；
// 速率 = completion_tokens ÷ 生成耗时秒。
func TestStreamingGenerationRate(t *testing.T) {
	up := startMockUpstream(t)
	defer up.Close()
	dir := t.TempDir()
	store, err := logsdb.Open(filepath.Join(dir, "test-log.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	mgr := config.NewManager(filepath.Join(dir, "cfg.yml"))
	if _, err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	state := core.NewState(mgr)
	defer state.Stop()
	err = state.Mutate(func(ns *core.Snapshot) {
		ns.Providers = []*config.Provider{{
			ID: "p1", Name: "p-openai", BaseURL: up.URL + "/v1", APIType: convert.FormatOpenAI,
			Enabled: true, Weight: 1,
			APIKeys: []config.ProviderAPIKey{{ID: "k1", Key: upstreamKey}},
			Models:  []config.ProviderModel{{Name: "m-openai"}},
		}}
		ns.APIKeys = []*config.ProxyKey{{ID: "pk1", Name: "e2e", Key: "sk-proxy-e2e-key", Enabled: true, IPWhitelist: []string{"*"}}}
		ns.LogEnabled = true // 测试栈显式开启日志（v1.8.1 起默认关闭）
	})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(state)
	h.SetLogStore(store)

	// 同格式流式（openai→openai，stream=true）：mock 首 token 延迟 60ms + 生成 120ms
	status, body, _ := postProxy(t, h, convert.FormatOpenAI, "m-openai",
		`{"model":"@MODEL@","messages":[{"role":"user","content":"你好"}],"stream":true}`, nil)
	if status != 200 || !strings.Contains(body, "[DONE]") {
		t.Fatalf("流式调用异常: status=%d body=%s", status, body)
	}

	var logs *logsdb.LogPage
	for i := 0; i < 50; i++ {
		logs, err = store.Query(logsdb.LogQuery{Page: 1, PageSize: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(logs.Logs) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(logs.Logs) == 0 {
		t.Fatal("流式调用日志未写入")
	}
	l := logs.Logs[0]
	if l.Status != "success" || l.CompletionTokens != 22 {
		t.Fatalf("流式日志状态或输出 token 异常: %+v", l)
	}
	// 总耗时 ≥ 首 token 延迟 + 生成阶段（放宽到 140ms 容忍调度误差）
	if l.DurationMs < 140 {
		t.Errorf("流式总耗时异常: %d ms（应含首 token 延迟与生成阶段）", l.DurationMs)
	}
	// 生成耗时 = 总耗时 - 首 token 延迟：应显著小于总耗时且 ≥ 生成阶段下限
	if l.GenerationMs <= 0 || l.GenerationMs >= l.DurationMs {
		t.Errorf("流式生成耗时应为 (0, 总耗时) 区间: gen=%d dur=%d", l.GenerationMs, l.DurationMs)
	}
	if ttft := l.DurationMs - l.GenerationMs; ttft < 40 {
		t.Errorf("首 token 延迟应 ≥ 40ms: ttft=%d dur=%d gen=%d", ttft, l.DurationMs, l.GenerationMs)
	}
	wantRate := 22.0 / (float64(l.GenerationMs) / 1000.0)
	if l.TokensPerSecond <= 0 || math.Abs(l.TokensPerSecond-wantRate) > 1e-6 {
		t.Errorf("流式输出速率异常: got %v, want %v (22 tokens / %d ms)", l.TokensPerSecond, wantRate, l.GenerationMs)
	}
}

// TestFakeStreamingRate 伪流式（缓冲型）上游回归：上游把整个 SSE 缓冲到最后一次性发出时，
// 响应体一次性到达（ReadChunks=1），生成耗时回退为总耗时，输出速率仍应正常显示（>0），
// 而不是因 gen=0 被记为 0（前端显示 -）。
func TestFakeStreamingRate(t *testing.T) {
	allSSE := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":100,\"total_tokens\":105}}\n\n" +
		"data: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond) // 生成期间不发任何字节（伪流式）
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, allSSE) // 一次性写出全部
	}))
	defer up.Close()
	dir := t.TempDir()
	store, err := logsdb.Open(filepath.Join(dir, "test-log.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	mgr := config.NewManager(filepath.Join(dir, "cfg.yml"))
	if _, err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	state := core.NewState(mgr)
	defer state.Stop()
	err = state.Mutate(func(ns *core.Snapshot) {
		ns.Providers = []*config.Provider{{
			ID: "p1", Name: "p-fake", BaseURL: up.URL + "/v1", APIType: convert.FormatOpenAI,
			Enabled: true, Weight: 1,
			APIKeys: []config.ProviderAPIKey{{ID: "k1", Key: upstreamKey}},
			Models:  []config.ProviderModel{{Name: "m-openai"}},
		}}
		ns.APIKeys = []*config.ProxyKey{{ID: "pk1", Name: "e2e", Key: "sk-proxy-e2e-key", Enabled: true, IPWhitelist: []string{"*"}}}
		ns.LogEnabled = true // 测试栈显式开启日志（v1.8.1 起默认关闭）
	})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(state)
	h.SetLogStore(store)

	status, body, _ := postProxy(t, h, convert.FormatOpenAI, "m-openai",
		`{"model":"@MODEL@","messages":[{"role":"user","content":"你好"}],"stream":true}`, nil)
	if status != 200 || !strings.Contains(body, "[DONE]") {
		t.Fatalf("伪流式调用异常: status=%d body=%s", status, body)
	}

	var logs *logsdb.LogPage
	for i := 0; i < 50; i++ {
		logs, err = store.Query(logsdb.LogQuery{Page: 1, PageSize: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(logs.Logs) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(logs.Logs) == 0 {
		t.Fatal("伪流式调用日志未写入")
	}
	l := logs.Logs[0]
	if l.Status != "success" || l.CompletionTokens != 100 {
		t.Fatalf("伪流式日志状态或输出 token 异常: %+v", l)
	}
	// 修复点：一次性到达时回退总耗时，速率可正常计算（旧实现 gen=0 → 速率 0 → 前端显示 -）
	if l.GenerationMs <= 0 {
		t.Errorf("伪流式生成耗时不应为 0（应回退总耗时）: gen=%d dur=%d", l.GenerationMs, l.DurationMs)
	}
	if l.TokensPerSecond <= 0 {
		t.Errorf("伪流式输出速率不应为 0: gen=%d dur=%d rate=%v", l.GenerationMs, l.DurationMs, l.TokensPerSecond)
	}
	wantRate := 100.0 / (float64(l.GenerationMs) / 1000.0)
	if math.Abs(l.TokensPerSecond-wantRate) > 1e-6 {
		t.Errorf("伪流式输出速率异常: got %v, want %v (100 tokens / %d ms)", l.TokensPerSecond, wantRate, l.GenerationMs)
	}
}

// TestModelNotFoundLogged 模型未匹配的调用也记录失败日志（状态 failure、原因"未匹配到对应模型"）。
func TestModelNotFoundLogged(t *testing.T) {
	up := startMockUpstream(t)
	defer up.Close()
	h, _, _, cleanup := newTestStack(t, up.URL)
	defer cleanup()
	dir := t.TempDir()
	store, err := logsdb.Open(filepath.Join(dir, "test-log.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h.SetLogStore(store)

	// 未知模型 → 404（响应行为不变）
	status, body, _ := postProxy(t, h, convert.FormatOpenAI, "m-not-exist",
		`{"model":"@MODEL@","messages":[{"role":"user","content":"hi"}]}`, nil)
	if status != 404 || !strings.Contains(body, "model_not_found") {
		t.Fatalf("status=%d body=%s", status, body)
	}

	// 等待异步日志落库
	var logs *logsdb.LogPage
	for i := 0; i < 50; i++ {
		logs, err = store.Query(logsdb.LogQuery{Page: 1, PageSize: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(logs.Logs) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if logs == nil || len(logs.Logs) == 0 {
		t.Fatal("模型未匹配的调用未记录日志")
	}
	l := logs.Logs[0]
	if l.Status != "failure" {
		t.Errorf("日志状态应为 failure: %s", l.Status)
	}
	if l.ErrorMsg == nil || *l.ErrorMsg != "未匹配到对应模型" {
		t.Errorf("错误原因异常: %v", l.ErrorMsg)
	}
	if l.RequestedModel != "m-not-exist" {
		t.Errorf("调用模型记录异常: %s", l.RequestedModel)
	}
	if l.APIKeyID != "pk1" || l.APIKey != "sk-proxy-e2e-key" {
		t.Errorf("代理 Key 记录异常: %s/%s", l.APIKeyID, l.APIKey)
	}
	if l.Provider != "" {
		t.Errorf("未尝试任何供应商时 Provider 应为空: %q", l.Provider)
	}
	if l.RetryCount != 0 {
		t.Errorf("重试次数应为 0: %d", l.RetryCount)
	}
	// 详情：client_resp_status 记录返回给客户端的 404，客户端请求 URL/头/体已记录（四组数据之一）
	detail, err := store.Get(l.ID)
	if err != nil || detail == nil {
		t.Fatalf("日志详情查询失败: %v", err)
	}
	if detail.ClientRespStatus != http.StatusNotFound {
		t.Errorf("client_resp_status 应为 404: %d", detail.ClientRespStatus)
	}
	if !strings.Contains(detail.ClientRespBody, "does not exist") {
		t.Errorf("client_resp_body 应记录返回给客户端的错误响应: %s", detail.ClientRespBody)
	}
	if !strings.Contains(detail.ClientReqURL, "/v1/") {
		t.Errorf("client_req_url 应记录客户端请求路径: %s", detail.ClientReqURL)
	}
	if !strings.Contains(detail.ReqBody, "m-not-exist") {
		t.Errorf("请求体未记录: %s", detail.ReqBody)
	}
	if detail.ReqMethod != http.MethodPost {
		t.Errorf("请求方法未记录: %s", detail.ReqMethod)
	}
	if detail.ReqHeaders == "" || !strings.Contains(detail.ReqHeaders, "sk-proxy-e2e-key") {
		t.Errorf("请求头未记录: %s", detail.ReqHeaders)
	}
}

// startAlwaysFailUpstream 启动恒返回 500 的 mock 上游（冷却池测试用）。
func startAlwaysFailUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"mock internal error","type":"server_error"}}`))
	}))
}

// TestCooldownRetryThreshold 冷却池重试次数阈值（端到端）：
// 同一供应商连续 N 轮全部 Key 失败后才入池，入池后路由不再选中（全部冷却 → 503）。
func TestCooldownRetryThreshold(t *testing.T) {
	up := startAlwaysFailUpstream(t)
	defer up.Close()
	h, state, _, cleanup := newTestStack(t, up.URL)
	defer cleanup()

	// 重试次数=2：第一轮失败仅累计，第二轮失败才入池
	if err := state.MutateSettings(func(ns *core.Snapshot) {
		ns.CooldownEnabled = true
		ns.CooldownSeconds = 60
		ns.CooldownRetryCount = 2
	}); err != nil {
		t.Fatal(err)
	}

	reqBody := `{"model":"@MODEL@","messages":[{"role":"user","content":"hi"}]}`

	// 第 1 轮：503（无可用供应商），但不应入池
	status, body, _ := postProxy(t, h, convert.FormatOpenAI, "m-openai", reqBody, nil)
	if status != 503 || !strings.Contains(body, "all providers failed") {
		t.Fatalf("第 1 轮应 503: status=%d body=%s", status, body)
	}
	if n := state.Cooldown.Size(); n != 0 {
		t.Fatalf("第 1 轮失败不应入池, pool size=%d", n)
	}

	// 第 2 轮：再次全部 Key 失败 → 达到阈值入池
	status, _, _ = postProxy(t, h, convert.FormatOpenAI, "m-openai", reqBody, nil)
	if status != 503 {
		t.Fatalf("第 2 轮应 503: status=%d", status)
	}
	if n := state.Cooldown.Size(); n != 1 {
		t.Fatalf("第 2 轮失败后应入池, pool size=%d", n)
	}

	// 第 3 轮：唯一供应商冷却中 → 503 且提示 cooling down
	status, body, _ = postProxy(t, h, convert.FormatOpenAI, "m-openai", reqBody, nil)
	if status != 503 || !strings.Contains(body, "cooling down") {
		t.Fatalf("第 3 轮应提示全部冷却: status=%d body=%s", status, body)
	}
	if n := state.Cooldown.Size(); n != 1 {
		t.Fatalf("冷却期重复失败不应重复入池, pool size=%d", n)
	}

	// 冷却池禁用：过滤直通，不再提示 cooling down（池中旧条目不再生效）
	if err := state.MutateSettings(func(ns *core.Snapshot) { ns.CooldownEnabled = false }); err != nil {
		t.Fatal(err)
	}
	status, body, _ = postProxy(t, h, convert.FormatOpenAI, "m-openai", reqBody, nil)
	if status != 503 || strings.Contains(body, "cooling down") {
		t.Fatalf("禁用冷却池后不应提示 cooling down: status=%d body=%s", status, body)
	}
}

// ===== 流式实时转发（v1.7.0：stream=true 边收边转发）=====

// startSlowStreamUpstream 启动慢速流式 mock 上游（openai 格式）：
// TTFT 200ms，之后每 300ms 一帧，共 3 帧 text + finish，总时长约 1100ms+。
func startSlowStreamUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		time.Sleep(200 * time.Millisecond) // TTFT
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-slow\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"m-openai\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"帧%d\"},\"finish_reason\":null}]}\n\n", i)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(300 * time.Millisecond)
		}
		fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-slow\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"m-openai\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":6,\"total_tokens\":7}}\n\n")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

// readRealtimeSSE 以真实 HTTP 客户端请求代理流式端点，返回（首字节耗时, 总耗时, 响应体, 状态码）。
func readRealtimeSSE(t *testing.T, ts *httptest.Server, path, body string) (time.Duration, time.Duration, string, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-proxy-e2e-key")
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var firstAt time.Duration
	var total []byte
	buf := make([]byte, 8*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if firstAt == 0 {
				firstAt = time.Since(start)
			}
			total = append(total, buf[:n]...)
		}
		if rerr != nil {
			break
		}
	}
	return firstAt, time.Since(start), string(total), resp.StatusCode
}

// TestStreamingRealtimePassthrough 同格式流式实时转发：客户端首字节应远早于流总时长
// （旧全缓冲实现下首字节须等全部生成完成，与本用例断言不符）。
func TestStreamingRealtimePassthrough(t *testing.T) {
	up := startSlowStreamUpstream(t)
	defer up.Close()
	h, _, _, cleanup := newTestStack(t, up.URL)
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	firstAt, totalDur, body, status := readRealtimeSSE(t, ts, "/v1/chat/completions",
		`{"model":"m-openai","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if status != 200 || !strings.Contains(body, "[DONE]") {
		t.Fatalf("status=%d body=%s", status, body)
	}
	// 上游 TTFT=200ms，总时长约 1100ms+；首字节应 < 600ms（远早于总时长）
	if firstAt > 600*time.Millisecond {
		t.Errorf("同格式流式首字节 %v 过晚（总时长 %v），实时转发未生效", firstAt, totalDur)
	}
	if totalDur < 1000*time.Millisecond {
		t.Errorf("总时长 %v 异常（应等待上游全部帧）", totalDur)
	}
}

// TestStreamingRealtimeConvert 跨格式流式实时转换：同样验证首字节实时到达。
func TestStreamingRealtimeConvert(t *testing.T) {
	up := startSlowStreamUpstream(t)
	defer up.Close()
	h, _, _, cleanup := newTestStack(t, up.URL)
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	firstAt, totalDur, body, status := readRealtimeSSE(t, ts, "/v1/messages",
		`{"model":"m-openai","messages":[{"role":"user","content":"hi"}],"max_tokens":50,"stream":true}`)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	// anthropic 客户端：转换后的事件序列 + 上游文本
	for _, want := range []string{"event: message_start", "event: content_block_delta", "event: message_stop", "帧1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("Anthropic SSE 缺少 %s: %s", want, body)
		}
	}
	if firstAt > 600*time.Millisecond {
		t.Errorf("跨格式流式首字节 %v 过晚（总时长 %v），实时转换未生效", firstAt, totalDur)
	}
}

// TestStreamFirstEventErrorRetry 上游 200 + 首事件为错误 JSON（OpenAI 兼容网关延迟报错）：
// 跨格式流式路径在首事件阶段识别错误，未向客户端写出任何字节 → 重试换 Key 成功。
// （同格式为字节级透传语义，上游错误流原样透给客户端，与直连行为一致，不做拦截。）
func TestStreamFirstEventErrorRetry(t *testing.T) {
	// 计数器模式：第一个到达的请求（无论哪个 Key，Key 顺序随机）返回 200+错误流，
	// 之后返回正常流——保证"首事件错误 → 未写客户端 → 换 Key 重试成功"路径必然被测到且结果确定。
	var mu sync.Mutex
	first := true
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		isFirst := first
		first = false
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if isFirst {
			// 200 + 流首事件即错误
			_, _ = w.Write([]byte(`data: {"error":{"message":"mock overloaded","type":"server_error"}}` + "\n\n"))
			return
		}
		fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-ok\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"m-openai\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好的\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-ok\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"m-openai\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8}}\n\n")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up.Close()

	dir := t.TempDir()
	store, err := logsdb.Open(filepath.Join(dir, "test-log.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mgr := config.NewManager(filepath.Join(dir, "cfg.yml"))
	if _, err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	state := core.NewState(mgr)
	defer state.Stop()
	err = state.Mutate(func(ns *core.Snapshot) {
		ns.Providers = []*config.Provider{{
			ID: "p1", Name: "p-2keys", BaseURL: up.URL + "/v1", APIType: convert.FormatOpenAI,
			Enabled: true, Weight: 1,
			APIKeys: []config.ProviderAPIKey{
				{ID: "k1", Key: "sk-first-key"},
				{ID: "k2", Key: upstreamKey},
			},
			Models: []config.ProviderModel{{Name: "m-openai"}},
		}}
		ns.APIKeys = []*config.ProxyKey{{ID: "pk1", Name: "e2e", Key: "sk-proxy-e2e-key", Enabled: true, IPWhitelist: []string{"*"}}}
		ns.LogEnabled = true // 测试栈显式开启日志（v1.8.1 起默认关闭）
	})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(state)
	h.SetLogStore(store)

	status, body, _ := postProxy(t, h, convert.FormatAnthropic, "m-openai",
		`{"model":"@MODEL@","messages":[{"role":"user","content":"hi"}],"max_tokens":50,"stream":true}`, nil)
	// 跨格式：Anthropic 客户端收到的是转换后 SSE；错误事件不应出现
	if status != 200 || !strings.Contains(body, "message_start") || !strings.Contains(body, "好的") || !strings.Contains(body, "message_stop") {
		t.Fatalf("首事件错误后应重试成功: status=%d body=%s", status, body)
	}
	if strings.Contains(body, "mock overloaded") {
		t.Errorf("错误事件不应透给客户端: %s", body)
	}
	// 日志应为成功且尝试数=2（坏 Key 失败 1 次 + 好 Key 成功）
	waitForLog(t, store)
	logs, _ := store.Query(logsdb.LogQuery{Page: 1, PageSize: 10})
	if len(logs.Logs) != 1 || logs.Logs[0].Status != "success" {
		t.Fatalf("日志应恰 1 条成功: %+v", logs.Logs)
	}
	if logs.Logs[0].RetryCount != 1 {
		t.Errorf("重试次数应为 1（坏 Key 1 次 + 好 Key 成功）: %+v", logs.Logs[0])
	}
	if logs.Logs[0].CompletionTokens != 5 {
		t.Errorf("usage 提取异常: %+v", logs.Logs[0])
	}
}

// TestStreamClientDisconnect 客户端中途断开：记 failure 日志（context canceled）且不 panic。
func TestStreamClientDisconnect(t *testing.T) {
	up := startSlowStreamUpstream(t)
	defer up.Close()
	dir := t.TempDir()
	store, err := logsdb.Open(filepath.Join(dir, "test-log.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mgr := config.NewManager(filepath.Join(dir, "cfg.yml"))
	if _, err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	state := core.NewState(mgr)
	defer state.Stop()
	err = state.Mutate(func(ns *core.Snapshot) {
		ns.Providers = []*config.Provider{{
			ID: "p1", Name: "p-openai", BaseURL: up.URL + "/v1", APIType: convert.FormatOpenAI,
			Enabled: true, Weight: 1,
			APIKeys: []config.ProviderAPIKey{{ID: "k1", Key: upstreamKey}},
			Models:  []config.ProviderModel{{Name: "m-openai"}},
		}}
		ns.APIKeys = []*config.ProxyKey{{ID: "pk1", Name: "e2e", Key: "sk-proxy-e2e-key", Enabled: true, IPWhitelist: []string{"*"}}}
		ns.LogEnabled = true // 测试栈显式开启日志（v1.8.1 起默认关闭）
	})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(state)
	h.SetLogStore(store)
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m-openai","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-proxy-e2e-key")
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8*1024)
	// 读到首帧后立即断开（上游还在继续生成）
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	waitForLog(t, store)
	logs, _ := store.Query(logsdb.LogQuery{Page: 1, PageSize: 10})
	if len(logs.Logs) == 0 {
		t.Fatal("客户端断开后应记日志")
	}
	l := logs.Logs[0]
	if l.Status != "failure" {
		t.Fatalf("客户端断开应记 failure: %+v", l)
	}
	// v1.7.1 起，客户端断开在流转发阶段产生的错误被统一归类：
	// watchdog 关闭上游 Body → "客户端断开"；或写入先失败 → "写入客户端失败: ..."。
	if l.ErrorMsg == nil {
		t.Fatalf("客户端断开后日志应有错误信息: %+v", l)
	}
	msg := *l.ErrorMsg
	if !strings.Contains(msg, "客户端断开") && !strings.Contains(msg, "写入客户端失败") && !strings.Contains(msg, "读取上游响应失败") {
		t.Errorf("错误信息应归类为客户端断开/读写中断: %s", msg)
	}
}

// TestCoolingDownLogConsistency 冷却池全入池场景：日志 error_msg 与返回给客户端的消息一致
// （修复 writeFailureLog 消息不一致：旧实现日志误记 all providers failed (no provider attempted)）。
func TestCoolingDownLogConsistency(t *testing.T) {
	up := startAlwaysFailUpstream(t)
	defer up.Close()
	dir := t.TempDir()
	store, err := logsdb.Open(filepath.Join(dir, "test-log.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mgr := config.NewManager(filepath.Join(dir, "cfg.yml"))
	if _, err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	state := core.NewState(mgr)
	defer state.Stop()
	err = state.Mutate(func(ns *core.Snapshot) {
		ns.Providers = []*config.Provider{{
			ID: "p1", Name: "p-fail", BaseURL: up.URL + "/v1", APIType: convert.FormatOpenAI,
			Enabled: true, Weight: 1,
			APIKeys: []config.ProviderAPIKey{{ID: "k1", Key: upstreamKey}},
			Models:  []config.ProviderModel{{Name: "m-openai"}},
		}}
		ns.APIKeys = []*config.ProxyKey{{ID: "pk1", Name: "e2e", Key: "sk-proxy-e2e-key", Enabled: true, IPWhitelist: []string{"*"}}}
		ns.LogEnabled = true // 测试栈显式开启日志（v1.8.1 起默认关闭）
		ns.CooldownEnabled = true
		ns.CooldownSeconds = 60
		ns.CooldownRetryCount = 1
	})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(state)
	h.SetLogStore(store)

	reqBody := `{"model":"@MODEL@","messages":[{"role":"user","content":"hi"}]}`
	// 第 1 次失败（retry_count=1 直接入池）
	postProxy(t, h, convert.FormatOpenAI, "m-openai", reqBody, nil)
	// 第 2 次：全部冷却 → 503
	status, body, _ := postProxy(t, h, convert.FormatOpenAI, "m-openai", reqBody, nil)
	if status != 503 || !strings.Contains(body, "cooling down") {
		t.Fatalf("应提示 cooling down: status=%d body=%s", status, body)
	}
	waitNoMoreLogs(t, store, 2)
	logs, _ := store.Query(logsdb.LogQuery{Page: 1, PageSize: 10})
	if len(logs.Logs) != 2 {
		t.Fatalf("应有 2 条日志: %d", len(logs.Logs))
	}
	// 最新一次（cooling down 场景）：日志与客户端消息一致（时间戳为秒级，两条同秒时顺序不定，遍历查找）
	found := false
	for _, l := range logs.Logs {
		if l.ErrorMsg != nil && strings.Contains(*l.ErrorMsg, "cooling down") {
			found = true
		}
	}
	if !found {
		t.Errorf("日志错误消息应与客户端一致（含 cooling down）: %+v", logs.Logs)
	}
}

// TestStreamFallbackFullBody 上游忽略 stream 参数返回完整 JSON（部分兼容网关行为）：
// 跨格式流式应兜底合成客户端格式 SSE（而非空流）。
func TestStreamFallbackFullBody(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 无论是否 stream=true，一律返回非流式 JSON
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-json","object":"chat.completion","created":1700000000,"model":"m-openai",
			"choices":[{"index":0,"message":{"role":"assistant","content":"兜底内容"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":9,"completion_tokens":12,"total_tokens":21}}`))
	}))
	defer up.Close()
	h, _, _, cleanup := newTestStack(t, up.URL)
	defer cleanup()

	// Anthropic 客户端 stream=true → openai 上游返回 JSON → 兜底合成 Anthropic SSE
	status, body, hdr := postProxy(t, h, convert.FormatAnthropic, "m-openai",
		`{"model":"@MODEL@","messages":[{"role":"user","content":"hi"}],"max_tokens":50,"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	for _, want := range []string{"event: message_start", "event: content_block_delta", "兜底内容", "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Errorf("兜底 SSE 缺少 %s: %s", want, body)
		}
	}
	p, c, _ := convert.ExtractUsage([]byte(body), true)
	if p != 9 || c != 12 {
		t.Errorf("兜底 SSE usage 异常: %d/%d", p, c)
	}
}

// waitForLog 轮询等待异步日志落库。
func waitForLog(t *testing.T, store *logsdb.LogStore) *logsdb.LogPage {
	t.Helper()
	for i := 0; i < 100; i++ {
		logs, err := store.Query(logsdb.LogQuery{Page: 1, PageSize: 50})
		if err != nil {
			t.Fatal(err)
		}
		if len(logs.Logs) > 0 {
			return logs
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// waitNoMoreLogs 轮询等待日志条数达到 want（异步写入可能分批落库）。
func waitNoMoreLogs(t *testing.T, store *logsdb.LogStore, want int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		logs, err := store.Query(logsdb.LogQuery{Page: 1, PageSize: 50})
		if err != nil {
			t.Fatal(err)
		}
		if len(logs.Logs) >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
