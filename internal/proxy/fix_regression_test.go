package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llmproxy/internal/config"
	"llmproxy/internal/convert"
	"llmproxy/internal/core"
	"llmproxy/internal/logsdb"
)

// v1.7.6 修复回归测试：M2（模型白名单别名归一）/ M3（非流式客户端断开）/
// M5①（跨格式流式 200+无法解析响应体按失败处理）。

// newFixStack 搭建代理栈（openai 类型上游；keys 数量与代理 Key 可用模型可配置）。
func newFixStack(t *testing.T, upstreamURL string, keys int, allowedModels []string) (*Handler, *logsdb.LogStore, func()) {
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
	aks := []config.ProviderAPIKey{}
	for i := 0; i < keys; i++ {
		aks = append(aks, config.ProviderAPIKey{ID: fmt.Sprintf("k%d", i+1), Key: fmt.Sprintf("sk-fix-%d", i+1)})
	}
	err = state.Mutate(func(ns *core.Snapshot) {
		ns.Providers = []*config.Provider{{
			ID: "p1", Name: "p-fix", BaseURL: upstreamURL + "/v1", APIType: convert.FormatOpenAI,
			Enabled: true, Weight: 1, APIKeys: aks,
			Models: []config.ProviderModel{{Name: "m-openai", Aliases: []string{"m-alias"}}},
		}}
		ns.APIKeys = []*config.ProxyKey{{
			ID: "pk1", Name: "e2e", Key: "sk-proxy-e2e-key", Enabled: true,
			IPWhitelist: []string{"*"}, Models: allowedModels,
		}}
		ns.CooldownEnabled = false
		ns.LogEnabled = true // 测试栈显式开启日志（v1.8.1 起默认关闭）
	})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(state)
	h.SetLogStore(store)
	cleanup := func() { state.Stop(); _ = store.Close() }
	return h, store, cleanup
}

// fixCall 向代理发起一次调用，返回状态码与响应体。
func fixCall(t *testing.T, ts *httptest.Server, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-proxy-e2e-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const fixCCBody = `{"model":%s,"messages":[{"role":"user","content":"hi"}]}`

// TestModelWhitelistAliasAllowed M2 回归：代理 Key 可用模型配置真实名时，
// 客户端用该模型的合法别名调用应放行（旧实现按原始串字面匹配，别名被误 403）。
func TestModelWhitelistAliasAllowed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()
	h, store, cleanup := newFixStack(t, up.URL, 1, []string{"m-openai"})
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	// 真实名：放行
	if code, body := fixCall(t, ts, "/v1/chat/completions", fmt.Sprintf(fixCCBody, `"m-openai"`)); code != 200 {
		t.Fatalf("真实名应 200，实际 %d: %s", code, body)
	}
	// 别名（解析后真实名命中白名单）：也应放行（M2 修复点）
	if code, body := fixCall(t, ts, "/v1/chat/completions", fmt.Sprintf(fixCCBody, `"m-alias"`)); code != 200 {
		t.Fatalf("合法别名应放行（白名单按解析后真实名归一匹配），实际 %d: %s", code, body)
	}
	// 其他模型：仍应 403
	if code, _ := fixCall(t, ts, "/v1/chat/completions", fmt.Sprintf(fixCCBody, `"other"`)); code != 403 {
		t.Fatalf("白名单外模型应 403，实际 %d", code)
	}
	_ = store
}

// TestNonStreamClientDisconnectClassification M3 回归：非流式请求客户端主动断开后，
// 代理不应把客户端断开误判为上游失败而烧光全部 Key 重试槽位，也不应把日志记成
// 「all providers failed: ... context canceled」——应与流式路径同口径记「客户端断开」。
func TestNonStreamClientDisconnectClassification(t *testing.T) {
	var upstreamHits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		time.Sleep(2 * time.Second) // 制造断开窗口
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()
	// 3 条 Key：旧实现会全部烧掉
	h, store, cleanup := newFixStack(t, up.URL, 3, nil)
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(fmt.Sprintf(fixCCBody, `"m-openai"`)))
	req.Header.Set("Authorization", "Bearer sk-proxy-e2e-key")
	go func() {
		resp, err := (&http.Client{}).Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	for atomic.LoadInt32(&upstreamHits) == 0 { // 等首个上游请求已发出
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	cancel()                            // 模拟客户端主动断开
	time.Sleep(1200 * time.Millisecond) // 等 handler 收尾并写日志

	page := waitForLog(t, store)
	if len(page.Logs) != 1 {
		t.Fatalf("应恰 1 条日志，实际 %d", len(page.Logs))
	}
	l := page.Logs[0]
	if l.Status != "failure" {
		t.Fatalf("客户端断开应记 failure，实际 %s", l.Status)
	}
	if l.ErrorMsg == nil || !strings.Contains(*l.ErrorMsg, "客户端断开") {
		t.Fatalf("错误应归类为「客户端断开」（旧实现误记 all providers failed: context canceled），实际 %v", l.ErrorMsg)
	}
	if l.RetryCount != 0 {
		t.Fatalf("客户端断开不应烧重试槽位（retry_count 应 0），实际 %d", l.RetryCount)
	}
	if atomic.LoadInt32(&upstreamHits) != 1 {
		t.Fatalf("上游应只收到 1 次请求（不烧重试），实际 %d", atomic.LoadInt32(&upstreamHits))
	}
}

// TestCrossFmtStreamUnparsable200 M5① 回归：跨格式流式时上游返回 200 + 完整错误 JSON
// （非 SSE），代理应按失败处理并重试，穷尽后归一化 503——不得向客户端伪造
// 「成功」的空事件流并记 success。
func TestCrossFmtStreamUnparsable200(t *testing.T) {
	var upstreamHits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		w.Header().Set("Content-Type", "application/json")
		// 兼容网关对 stream=true 仍返回完整错误 JSON 的场景
		_, _ = w.Write([]byte(`{"error":{"message":"Insufficient quota","type":"insufficient_quota","code":"insufficient_quota"}}`))
	}))
	defer up.Close()
	h, store, cleanup := newFixStack(t, up.URL, 1, nil)
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	code, body := fixCall(t, ts, "/v1/responses",
		`{"model":"m-openai","input":"hi","stream":true}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("穷尽重试后应 503（旧实现伪造成功空流），实际 %d: %s", code, body)
	}
	if strings.Contains(body, "response.completed") || strings.Contains(body, "\"output_text\"") {
		t.Fatalf("不应向客户端输出伪造的成功终止流: %s", body)
	}
	if atomic.LoadInt32(&upstreamHits) < 1 {
		t.Fatalf("上游应被请求过，实际 %d", atomic.LoadInt32(&upstreamHits))
	}
	page := waitForLog(t, store)
	if len(page.Logs) != 1 {
		t.Fatalf("应恰 1 条日志，实际 %d", len(page.Logs))
	}
	l := page.Logs[0]
	if l.Status != "failure" {
		t.Fatalf("应记 failure（旧实现记 success），实际 %s", l.Status)
	}
	if l.ErrorMsg == nil || !strings.Contains(*l.ErrorMsg, "无法解析的 200") {
		t.Fatalf("错误信息应说明 200 响应无法解析，实际 %v", l.ErrorMsg)
	}
}

// TestReqBodyTruncatedInLog M4 回归：超长客户端请求体入库时应按 1MB 截断
// （与其他三组数据同口径，需求 4.3）。
func TestReqBodyTruncatedInLog(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()
	h, store, cleanup := newFixStack(t, up.URL, 1, nil)
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	// 构造 1.1MB 的请求体（长用户消息）
	big := strings.Repeat("A", 1100*1024)
	body := fmt.Sprintf(`{"model":"m-openai","messages":[{"role":"user","content":%s}]}`, mustJSON(big))
	if _, respBody := fixCall(t, ts, "/v1/chat/completions", body); len(respBody) == 0 {
		t.Fatal("请求应成功")
	}
	page := waitForLog(t, store)
	if len(page.Logs) != 1 {
		t.Fatalf("应恰 1 条日志，实际 %d", len(page.Logs))
	}
	detail, err := store.Get(page.Logs[0].ID)
	if err != nil || detail == nil {
		t.Fatal("详情查询失败")
	}
	if len(detail.ReqBody) > (1<<20)+200 { // 截断后允许 "...(truncated)" 标记的少量超出
		t.Fatalf("req_body 应按 1MB 截断，实际长度 %d", len(detail.ReqBody))
	}
	if !strings.Contains(detail.ReqBody, "...(truncated)") {
		t.Fatalf("截断应带 ...(truncated) 标记，尾部: %q", detail.ReqBody[len(detail.ReqBody)-80:])
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
