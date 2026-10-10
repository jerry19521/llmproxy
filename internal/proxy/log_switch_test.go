package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmproxy/internal/core"
	"llmproxy/internal/logsdb"
)

// v1.0.0 回归：日志记录开关（log.enabled，默认关闭）。
// - 关闭：成功调用 / 失败调用 / 模型未匹配均不写日志（writeSuccessLog /
//   writeFailureLog / writeModelNotFoundLog 三路径全覆盖）；
// - 开启：各路径恢复写入；
// - 开关热切换：运行中 MutateSettings 切换后下一次调用立即跟随新状态。

// TestLogSwitchDisabledDropsAllWrites 关闭状态下成功调用与模型未匹配均不落库。
func TestLogSwitchDisabledDropsAllWrites(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()

	h, store, cleanup := newFixStack(t, up.URL, 1, nil)
	defer cleanup()
	// newFixStack 显式开启日志；此处显式关闭，验证开关的「关闭」语义
	if err := h.State.MutateSettings(func(ns *core.Snapshot) { ns.LogEnabled = false }); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	// 成功调用
	if code, body := fixCall(t, ts, "/v1/chat/completions", `{"model":"m-openai","messages":[{"role":"user","content":"hi"}]}`); code != 200 {
		t.Fatalf("调用应成功，实际 %d: %s", code, body)
	}
	// 模型未匹配（404 路径）
	if code, _ := fixCall(t, ts, "/v1/chat/completions", `{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`); code != 404 {
		t.Fatalf("未匹配模型应 404，实际 %d", code)
	}

	// 等待异步写入窗口（若有日志应已落库）
	time.Sleep(500 * time.Millisecond)
	page, err := store.Query(logsdb.LogQuery{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Logs) != 0 {
		t.Fatalf("开关关闭时不应写入任何日志，实际 %d 条: %+v", len(page.Logs), page.Logs)
	}
}

// TestLogSwitchEnabledWritesLog 开启状态下成功调用正常落库，热切换即时生效。
func TestLogSwitchEnabledWritesLog(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()

	h, store, cleanup := newFixStack(t, up.URL, 1, nil) // 栈内已显式开启日志
	defer cleanup()
	if !h.State.Snap().LogEnabled {
		t.Fatal("测试栈应开启日志")
	}
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	// 开启：成功调用 → 1 条
	if code, body := fixCall(t, ts, "/v1/chat/completions", `{"model":"m-openai","messages":[{"role":"user","content":"hi"}]}`); code != 200 {
		t.Fatalf("调用应成功，实际 %d: %s", code, body)
	}
	page := waitForLog(t, store)
	if len(page.Logs) != 1 {
		t.Fatalf("开启时应写 1 条日志，实际 %d", len(page.Logs))
	}
	if page.Logs[0].Status != "success" {
		t.Fatalf("应记 success，实际 %s", page.Logs[0].Status)
	}

	// 热切换为关闭：新调用不再写日志（不影响调用本身）
	if err := h.State.MutateSettings(func(ns *core.Snapshot) { ns.LogEnabled = false }); err != nil {
		t.Fatal(err)
	}
	if code, body := fixCall(t, ts, "/v1/chat/completions", `{"model":"m-openai","messages":[{"role":"user","content":"hi"}]}`); code != 200 {
		t.Fatalf("关闭后调用仍应成功（开关仅影响日志），实际 %d: %s", code, body)
	}
	time.Sleep(500 * time.Millisecond)
	page2, err := store.Query(logsdb.LogQuery{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Logs) != 1 {
		t.Fatalf("关闭后新调用不应追加日志，实际 %d 条", len(page2.Logs))
	}

	// 再热切换为开启：恢复写入
	if err := h.State.MutateSettings(func(ns *core.Snapshot) { ns.LogEnabled = true }); err != nil {
		t.Fatal(err)
	}
	if code, body := fixCall(t, ts, "/v1/chat/completions", `{"model":"m-openai","messages":[{"role":"user","content":"hi"}]}`); code != 200 {
		t.Fatalf("再次开启后调用应成功，实际 %d: %s", code, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		page3, _ := store.Query(logsdb.LogQuery{Page: 1, PageSize: 10})
		if len(page3.Logs) == 2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("再次开启后应恢复写入（总 2 条）")
}

// TestLogSwitchModelNotFoundPath 模型未匹配路径受开关控制：关闭不写、开启写 failure。
func TestLogSwitchModelNotFoundPath(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()

	h, store, cleanup := newFixStack(t, up.URL, 1, nil)
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	// 关闭：404 不写日志
	if err := h.State.MutateSettings(func(ns *core.Snapshot) { ns.LogEnabled = false }); err != nil {
		t.Fatal(err)
	}
	if code, _ := fixCall(t, ts, "/v1/chat/completions", `{"model":"no-such","messages":[{"role":"user","content":"hi"}]}`); code != 404 {
		t.Fatalf("未匹配应 404，实际 %d", code)
	}
	time.Sleep(300 * time.Millisecond)
	if page, _ := store.Query(logsdb.LogQuery{Page: 1, PageSize: 10}); len(page.Logs) != 0 {
		t.Fatalf("关闭时模型未匹配不应写日志，实际 %d 条", len(page.Logs))
	}

	// 开启：404 写 1 条 failure（错误原因「未匹配到对应模型」）
	if err := h.State.MutateSettings(func(ns *core.Snapshot) { ns.LogEnabled = true }); err != nil {
		t.Fatal(err)
	}
	if code, _ := fixCall(t, ts, "/v1/chat/completions", `{"model":"no-such","messages":[{"role":"user","content":"hi"}]}`); code != 404 {
		t.Fatalf("未匹配应 404，实际 %d", code)
	}
	page := waitForLog(t, store)
	if len(page.Logs) != 1 {
		t.Fatalf("开启时模型未匹配应写 1 条，实际 %d", len(page.Logs))
	}
	if page.Logs[0].Status != "failure" || page.Logs[0].ErrorMsg == nil || !strings.Contains(*page.Logs[0].ErrorMsg, "未匹配到对应模型") {
		t.Fatalf("模型未匹配日志内容异常: %+v", page.Logs[0])
	}
}

// TestLogSwitchFailurePathNoUpstream 全失败路径（writeFailureLog）受开关控制。
func TestLogSwitchFailurePathNoUpstream(t *testing.T) {
	// 上游始终 500：单 Key 穷尽后全失败
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer up.Close()

	h, store, cleanup := newFixStack(t, up.URL, 1, nil)
	defer cleanup()
	if err := h.State.MutateSettings(func(ns *core.Snapshot) {
		ns.LogEnabled = false
		ns.CooldownEnabled = false // 隔离冷却池影响
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	if code, _ := fixCall(t, ts, "/v1/chat/completions", `{"model":"m-openai","messages":[{"role":"user","content":"hi"}]}`); code != 503 {
		t.Fatalf("上游全失败应 503，实际 %d", code)
	}
	time.Sleep(500 * time.Millisecond)
	if page, _ := store.Query(logsdb.LogQuery{Page: 1, PageSize: 10}); len(page.Logs) != 0 {
		t.Fatalf("关闭时失败调用不应写日志，实际 %d 条", len(page.Logs))
	}

	// 开启后同场景写 failure
	if err := h.State.MutateSettings(func(ns *core.Snapshot) { ns.LogEnabled = true }); err != nil {
		t.Fatal(err)
	}
	if code, _ := fixCall(t, ts, "/v1/chat/completions", `{"model":"m-openai","messages":[{"role":"user","content":"hi"}]}`); code != 503 {
		t.Fatalf("上游全失败应 503，实际 %d", code)
	}
	page := waitForLog(t, store)
	if len(page.Logs) != 1 || page.Logs[0].Status != "failure" {
		t.Fatalf("开启后失败调用应写 1 条 failure，实际 %+v", page.Logs)
	}
}
