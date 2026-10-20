package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"llmproxy/internal/config"
	"llmproxy/internal/convert"
	"llmproxy/internal/core"
	"llmproxy/internal/logsdb"
)

// 本文件验证流式转发中途失败/中断的「不重试」守卫（需求 6.3 第 8 步 / 6.4）：
// 一旦已向客户端写出数据（响应已开始），后续任何失败（上游中途断流、
// 客户端断开、流中错误事件）都不得换 Key / 换供应商重试——
// 否则客户端会收到重复内容或损坏的 SSE 流。
//
// 验证手法：mock 上游计数请求次数；若被（错误地）重试，第二次请求返回
// 带污染标记 RETRY_LEAK 的内容，客户端一旦收到该标记即证明发生了误重试。

// newStreamGuardStack 搭建带 2 条 Key 的单供应商代理栈（openai 类型上游）。
func newStreamGuardStack(t *testing.T, upstreamURL string) (*Handler, *logsdb.LogStore, func()) {
	t.Helper()
	dir := t.TempDir()
	store, err := logsdb.Open(filepath.Join(dir, "test-log.db"))
	if err != nil {
		t.Fatal(err)
	}
	mgr := config.NewManager(filepath.Join(dir, "cfg.yml"))
	if _, err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	state := core.NewState(mgr)
	err = state.Mutate(func(ns *core.Snapshot) {
		ns.Providers = []*config.Provider{{
			ID: "p1", Name: "p-guard", BaseURL: upstreamURL + "/v1", APIType: convert.FormatOpenAI,
			Enabled: true, Weight: 1,
			// 两条 Key：若误重试会切换到第二条
			APIKeys: []config.ProviderAPIKey{
				{ID: "k1", Key: "sk-a", Enabled: true}, {ID: "k2", Key: "sk-b", Enabled: true},
			},
			Models: []config.ProviderModel{{Name: "m-openai"}},
		}}
		ns.APIKeys = []*config.ProxyKey{{ID: "pk1", Name: "e2e", Key: "sk-proxy-e2e-key", Enabled: true, IPWhitelist: []string{"*"}}}
		// 禁用冷却池：隔离变量，专注重试守卫本身
		ns.CooldownEnabled = false
		ns.LogEnabled = true // 测试栈显式开启日志
	})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(state)
	h.SetLogStore(store)
	cleanup := func() { state.Stop(); store.Close() }
	return h, store, cleanup
}

// retryLeakUpstream 首次请求：发送 PART1 后按 abortMode 中途失败；
// 若被再次请求（不应发生）：返回带 RETRY_LEAK 标记的完整数据。
func retryLeakUpstream(t *testing.T, calls *int64, abortMode string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(calls, 1) > 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"m-openai\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"RETRY_LEAK\"},\"finish_reason\":null}]}\n\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"m-openai\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"PART1\"},\"finish_reason\":null}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		switch abortMode {
		case "abort":
			// 模拟上游中途断流（网络异常/进程崩溃）
			panic(http.ErrAbortHandler)
		case "error_event":
			// 模拟 200 + 流中延迟报错（部分兼容网关行为）
			fmt.Fprint(w, "data: {\"error\":{\"message\":\"mid-stream boom\",\"type\":\"server_error\"}}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(50 * time.Millisecond)
		}
	}))
}

// assertNoRetry 断言：上游仅被请求 1 次、日志恰 1 条 failure、客户端响应含首段数据且无重试泄漏。
func assertNoRetry(t *testing.T, ts *httptest.Server, store *logsdb.LogStore, calls *int64,
	clientPath, clientBody, wantFirstChunk string, wantErrMsgContains []string) {
	t.Helper()

	_, _, body, status := readRealtimeSSE(t, ts, clientPath, clientBody)
	if status != 200 {
		t.Fatalf("首个有效数据到达后客户端应已是 200 响应，实际 %d（body=%s）", status, body)
	}
	if !contains(body, wantFirstChunk) {
		t.Fatalf("客户端应收到首段数据 %q，实际 body=%s", wantFirstChunk, body)
	}
	if contains(body, "RETRY_LEAK") {
		t.Fatalf("检测到重试泄漏：客户端收到了第二次请求的重复内容（body=%s）", body)
	}

	waitForLog(t, store)
	time.Sleep(300 * time.Millisecond) // 留出可能（不应发生）的第二条日志窗口
	logs, err := store.Query(logsdb.LogQuery{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs.Logs) != 1 {
		t.Fatalf("应恰写 1 条日志（最终状态），实际 %d 条：%+v", len(logs.Logs), logs.Logs)
	}
	l := logs.Logs[0]
	if l.Status != "failure" {
		t.Fatalf("已开始转发后中途失败应记 failure，实际 %s（%+v）", l.Status, l)
	}
	if l.RetryCount != 0 {
		t.Errorf("不应发生重试（retry_count 应为 0），实际 %d", l.RetryCount)
	}
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Errorf("上游应仅被请求 1 次（不得换 Key 重试），实际 %d 次", n)
	}
	if l.ErrorMsg == nil {
		t.Fatalf("失败日志应有错误信息: %+v", l)
	}
	for _, want := range wantErrMsgContains {
		if !contains(*l.ErrorMsg, want) {
			t.Errorf("错误信息应含 %q，实际 %q", want, *l.ErrorMsg)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestStreamGuardSameFmtMidAbort 同格式流式：上游先发一段数据（已透传客户端）后中途断流。
// 期望：不换 Key / 不换供应商重试；客户端只收到一份内容；记 1 条 failure 日志。
func TestStreamGuardSameFmtMidAbort(t *testing.T) {
	var calls int64
	up := retryLeakUpstream(t, &calls, "abort")
	defer up.Close()
	h, store, cleanup := newStreamGuardStack(t, up.URL)
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	assertNoRetry(t, ts, store, &calls, "/v1/chat/completions",
		`{"model":"m-openai","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		"PART1", []string{"读取上游响应失败"})
}

// TestStreamGuardCrossFmtMidAbort 跨格式流式（Anthropic 客户端 × OpenAI 上游）：
// 上游先发一段数据（已实时转换写给客户端）后中途断流。期望同上。
func TestStreamGuardCrossFmtMidAbort(t *testing.T) {
	var calls int64
	up := retryLeakUpstream(t, &calls, "abort")
	defer up.Close()
	h, store, cleanup := newStreamGuardStack(t, up.URL)
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	// 跨格式：上游 delta.content=PART1 会转换为 Anthropic text_delta 事件
	assertNoRetry(t, ts, store, &calls, "/v1/messages",
		`{"model":"m-openai","max_tokens":50,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
		"PART1", []string{"读取上游响应失败"})
}

// TestStreamGuardCrossFmtMidErrorEvent 跨格式流式：上游 200 + 先正常输出后流中出现错误事件。
// 已写出数据后不得重试（需求 6.3：已写出则断流记日志）。
func TestStreamGuardCrossFmtMidErrorEvent(t *testing.T) {
	var calls int64
	up := retryLeakUpstream(t, &calls, "error_event")
	defer up.Close()
	h, store, cleanup := newStreamGuardStack(t, up.URL)
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	assertNoRetry(t, ts, store, &calls, "/v1/messages",
		`{"model":"m-openai","max_tokens":50,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
		"PART1", []string{"mid-stream boom"})
}
