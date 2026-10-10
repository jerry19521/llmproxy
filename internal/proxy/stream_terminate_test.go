package proxy

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmproxy/internal/logsdb"
)

// 本文件验证「终止帧已送达后客户端断开」的正确归类（v1.0.0）：
// SSE 客户端（如 Codex）读完终止事件（response.completed / [DONE] / message_stop）
// 后即认为响应结束并关闭连接，属正常完成。此时代理若还在等上游剩余字节
// （[DONE] 后的 keep-alive 等），随后的读错误不应记为"客户端断开"失败，
// 本次调用应按成功收尾。
//
// 同时验证终止帧的延迟发出（evFinish 不再立即 finalize）：OpenAI CC 流的
// finish_reason chunk 之后到达的 usage chunk 必须体现在终止帧里。

// completeThenHangUpstream mock 上游：发送完整 CC 流（text → finish_reason →
// usage → [DONE]），随后保持连接打开（挂住不发 EOF），模拟 [DONE] 后
// keep-alive 挂住——转发循环只能因客户端断开（watchdog 关闭上游连接）而结束。
//
// 帧间固定 20ms 间隔（L15 修复）：真实流式上游逐帧到达；无间隔时高负载机器上
// 整个链路亚毫秒完成，duration_ms/generation_ms 毫秒级计时归零，
// 导致 TestStreamLogFourWayData 的 GenerationMs>0 断言偶发失败（flaky）。
func completeThenHangUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"m-openai\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"HELLO\"},\"finish_reason\":null}]}\n\n")
		flusher.Flush()
		time.Sleep(20 * time.Millisecond)
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"m-openai\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		flusher.Flush()
		time.Sleep(20 * time.Millisecond)
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"m-openai\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30}}\n\n")
		flusher.Flush()
		time.Sleep(20 * time.Millisecond)
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		// [DONE] 之后保持连接打开：等代理（因客户端断开）关闭连接
		<-r.Context().Done()
	}))
}

// readUntilMarker 读 SSE 直到出现 marker 即刻断开连接
// （模拟客户端收完终止帧后的正常关闭，不等上游 keep-alive 结束）。
func readUntilMarker(t *testing.T, ts *httptest.Server, path, body, marker string) (string, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-proxy-e2e-key")
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var total []byte
	buf := make([]byte, 8*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			total = append(total, buf[:n]...)
			if bytes.Contains(total, []byte(marker)) {
				resp.Body.Close() // 模拟客户端读完终止帧立即断开
				return string(total), resp.StatusCode
			}
		}
		if rerr != nil {
			break
		}
	}
	return string(total), resp.StatusCode
}

// assertCompletedSuccess 断言日志恰 1 条且为 success（无错误信息）。
func assertCompletedSuccess(t *testing.T, store *logsdb.LogStore) {
	t.Helper()
	waitForLog(t, store)
	time.Sleep(300 * time.Millisecond) // 留出额外写入窗口（不应有第二条）
	logs, err := store.Query(logsdb.LogQuery{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs.Logs) != 1 {
		t.Fatalf("应恰写 1 条日志，实际 %d 条：%+v", len(logs.Logs), logs.Logs)
	}
	l := logs.Logs[0]
	if l.Status != "success" {
		t.Fatalf("终止帧已送达后客户端断开应记 success，实际 %s（error=%v）", l.Status, l.ErrorMsg)
	}
	if l.ErrorMsg != nil && *l.ErrorMsg != "" {
		t.Errorf("成功日志不应有错误信息: %q", *l.ErrorMsg)
	}
}

// TestStreamCompletedThenClientCloseCrossFmt 跨格式流式（Responses 客户端 × OpenAI 上游）：
// 客户端读到 response.completed 后关闭连接（正常行为），上游连接仍挂着。
// 期望：记 success；终止帧包含 finish_reason 之后到达的 usage（延迟终止）。
func TestStreamCompletedThenClientCloseCrossFmt(t *testing.T) {
	up := completeThenHangUpstream(t)
	defer up.Close()
	h, store, cleanup := newStreamGuardStack(t, up.URL)
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	body, status := readUntilMarker(t, ts, "/v1/responses",
		`{"model":"m-openai","input":"hi","stream":true}`,
		"response.completed")
	if status != 200 || !strings.Contains(body, "response.completed") {
		t.Fatalf("status=%d body=%s", status, body)
	}
	// 终止帧须带上 finish_reason 之后、[DONE] 之前到达的 usage（延迟 finalize 的效果）
	if !strings.Contains(body, `"input_tokens":10`) || !strings.Contains(body, `"output_tokens":20`) {
		t.Fatalf("终止帧应包含 usage:\n%s", body)
	}
	assertCompletedSuccess(t, store)
}

// TestStreamCompletedThenClientCloseSameFmt 同格式流式透传：
// 客户端读到 data: [DONE] 后关闭连接，上游连接仍挂着。期望：记 success。
func TestStreamCompletedThenClientCloseSameFmt(t *testing.T) {
	up := completeThenHangUpstream(t)
	defer up.Close()
	h, store, cleanup := newStreamGuardStack(t, up.URL)
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	body, status := readUntilMarker(t, ts, "/v1/chat/completions",
		`{"model":"m-openai","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		"data: [DONE]")
	if status != 200 || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if !strings.Contains(body, `"prompt_tokens":10`) {
		t.Fatalf("透传流应含 usage chunk:\n%s", body)
	}
	assertCompletedSuccess(t, store)
}

// TestStreamLogFourWayData 跨格式流式：日志详情应完整记录四组请求/响应数据
// （客户端请求 / 代理→上游请求 / 上游响应 / 代理→客户端响应），认证头脱敏（v1.0.0）。
func TestStreamLogFourWayData(t *testing.T) {
	up := completeThenHangUpstream(t)
	defer up.Close()
	h, store, cleanup := newStreamGuardStack(t, up.URL)
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	// Responses 客户端（跨格式流式，代理转换为 OpenAI CC 发上游）
	body, status := readUntilMarker(t, ts, "/v1/responses",
		`{"model":"m-openai","input":"hi","stream":true}`,
		"response.completed")
	if status != 200 || !strings.Contains(body, "response.completed") {
		t.Fatalf("status=%d body=%s", status, body)
	}

	waitForLog(t, store)
	logs, err := store.Query(logsdb.LogQuery{Page: 1, PageSize: 10})
	if err != nil || len(logs.Logs) != 1 {
		t.Fatalf("应恰 1 条日志: err=%v n=%d", err, len(logs.Logs))
	}
	detail, err := store.Get(logs.Logs[0].ID)
	if err != nil || detail == nil {
		t.Fatalf("详情查询失败: %v", err)
	}

	// 组一：客户端 → 代理（Responses 原始请求）
	if detail.ClientReqURL != "/v1/responses" {
		t.Errorf("client_req_url = %q, want /v1/responses", detail.ClientReqURL)
	}
	if !strings.Contains(detail.ReqBody, `"input":"hi"`) {
		t.Errorf("req_body 应为客户端原始 Responses 请求: %s", detail.ReqBody)
	}
	if detail.ReqHeaders == "" || !strings.Contains(detail.ReqHeaders, "Authorization") {
		t.Errorf("req_headers 应记录: %s", detail.ReqHeaders)
	}

	// 组二：代理 → 上游（转换后的 OpenAI CC 请求；认证头脱敏）
	if !strings.Contains(detail.UpstreamURL, "/v1/chat/completions") {
		t.Errorf("upstream_url = %s", detail.UpstreamURL)
	}
	if !strings.Contains(detail.UpstreamReqBody, `"messages"`) {
		t.Errorf("upstream_req_body 应为转换后 CC 请求: %s", detail.UpstreamReqBody)
	}
	if !strings.Contains(detail.UpstreamReqBody, `"stream":true`) {
		t.Errorf("upstream_req_body 应含 stream=true: %s", detail.UpstreamReqBody)
	}
	if !strings.Contains(detail.UpstreamReqHeaders, "Authorization") {
		t.Errorf("upstream_req_headers 应记录: %s", detail.UpstreamReqHeaders)
	}
	if strings.Contains(detail.UpstreamReqHeaders, "sk-a") || strings.Contains(detail.UpstreamReqHeaders, "sk-b") {
		t.Errorf("上游认证头应脱敏: %s", detail.UpstreamReqHeaders)
	}

	// 组三：上游 → 代理
	if detail.RespStatus != 200 {
		t.Errorf("resp_status = %d, want 200", detail.RespStatus)
	}
	if !strings.Contains(detail.RespBody, "data: [DONE]") {
		t.Errorf("resp_body 应为上游响应副本: %s", detail.RespBody)
	}
	if detail.RespHeaders == "" {
		t.Errorf("resp_headers 应记录")
	}

	// 组四：代理 → 客户端
	if detail.ClientRespStatus != 200 {
		t.Errorf("client_resp_status = %d, want 200", detail.ClientRespStatus)
	}
	if !strings.Contains(detail.ClientRespHeaders, "text/event-stream") {
		t.Errorf("client_resp_headers 应含 event-stream: %s", detail.ClientRespHeaders)
	}
	if !strings.Contains(detail.ClientRespBody, "response.completed") {
		t.Errorf("client_resp_body 应为转换后输出: %s", detail.ClientRespBody)
	}

	// 速率口径：流式生成耗时 = 末字节 - 上游首字节；首字节已入库
	if detail.FirstByteMs < 0 {
		t.Errorf("first_byte_ms 应入库: %d", detail.FirstByteMs)
	}
	if detail.GenerationMs <= 0 || detail.GenerationMs > detail.DurationMs {
		t.Errorf("generation_ms 异常: gen=%d dur=%d", detail.GenerationMs, detail.DurationMs)
	}
}
