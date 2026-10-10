package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"llmproxy/internal/config"
	"llmproxy/internal/convert"
	"llmproxy/internal/core"
	"llmproxy/internal/logsdb"
)

// v1.8.2 代码审查修复回归测试：A1（坏 SSE 伪造成功）/ A2（流式断开烧重试）/
// A4（白名单首候选口径）/ C5（截断中文乱码）。

// auditProviderSpec 描述审计测试栈的一个供应商。
type auditProviderSpec struct {
	id, name, baseURL, apiType string
	keys                       int
	model                      string   // 真实模型名
	aliases                    []string // 别名
}

// newAuditStack 搭建多供应商审计测试栈（复用 fixCall 与 fixCCBody）。
// 代理 Key 固定为 sk-proxy-e2e-key，便于复用 fixCall；CooldownEnabled=false。
func newAuditStack(t *testing.T, providers []auditProviderSpec, whitelist []string) (*Handler, *logsdb.LogStore, func()) {
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
	var ps []*config.Provider
	for _, sp := range providers {
		aks := make([]config.ProviderAPIKey, 0, sp.keys)
		for i := 0; i < sp.keys; i++ {
			aks = append(aks, config.ProviderAPIKey{ID: fmt.Sprintf("%s-k%d", sp.id, i+1), Key: fmt.Sprintf("sk-%s-%d", sp.id, i+1)})
		}
		ps = append(ps, &config.Provider{
			ID: sp.id, Name: sp.name, BaseURL: sp.baseURL, APIType: sp.apiType,
			Enabled: true, Weight: 1, APIKeys: aks,
			Models: []config.ProviderModel{{Name: sp.model, Aliases: sp.aliases}},
		})
	}
	if err := state.Mutate(func(ns *core.Snapshot) {
		ns.Providers = ps
		ns.APIKeys = []*config.ProxyKey{{
			ID: "pk1", Name: "audit", Key: "sk-proxy-e2e-key", Enabled: true,
			IPWhitelist: []string{"*"}, Models: whitelist,
		}}
		ns.CooldownEnabled = false
		ns.LogEnabled = true
	}); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(state)
	h.SetLogStore(store)
	cleanup := func() { state.Stop(); _ = store.Close() }
	return h, store, cleanup
}

// TestBadSSEHtmlFailsNotFakesSuccess A1（V-P2）回归：跨格式流式时上游返回 200 + data: <html> 坏帧，
// 代理应按失败处理（可重试），穷尽后 503——不得向客户端伪造成功终止流并记 success。
func TestBadSSEHtmlFailsNotFakesSuccess(t *testing.T) {
	var upstreamHits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: <html>Bad Gateway Error Page</html>\n\n"))
	}))
	defer up.Close()
	h, store, cleanup := newAuditStack(t, []auditProviderSpec{{
		id: "p1", name: "badup", baseURL: up.URL, apiType: convert.FormatResponses, keys: 1, model: "testmodel",
	}}, nil)
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	code, body := fixCall(t, ts, "/v1/chat/completions",
		`{"model":"testmodel","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("穷尽重试后应 503（旧实现伪造成功空流），实际 %d: %s", code, body)
	}
	if strings.Contains(body, "[DONE]") || strings.Contains(body, "finish_reason") || strings.Contains(body, "chat.completion.chunk") {
		t.Fatalf("不应向客户端输出伪造的成功终止流: %s", body)
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
		t.Fatalf("错误应说明 200 响应无法解析，实际 %v", l.ErrorMsg)
	}
}

// TestStreamClientDisconnectBeforeFirstByteNoRetry A2（V-N1）回归：流式首字节前客户端断开，
// 代理不应对每个 Key 真实发起上游请求烧配额，应记 failure「客户端断开」、retry_count=0、上游仅 1 次请求。
func TestStreamClientDisconnectBeforeFirstByteNoRetry(t *testing.T) {
	var upstreamHits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		// 响应头立即到达，首帧延迟 1s：模拟「响应头已到、首帧在途」的阻塞窗口，
		// 确保 watchdog 关闭 body 时 Read 正阻塞等待首帧。
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(1 * time.Second)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"))
	}))
	defer up.Close()
	// 3 条 Key：旧实现对每条 Key 真实发起上游请求（共 3 次）
	h, store, cleanup := newAuditStack(t, []auditProviderSpec{{
		id: "p1", name: "slowup", baseURL: up.URL, apiType: convert.FormatOpenAI, keys: 3, model: "testmodel",
	}}, nil)
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"testmodel","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
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
	time.Sleep(150 * time.Millisecond) // 等代理进入首帧阻塞读后断开
	cancel()
	time.Sleep(1200 * time.Millisecond) // 等 handler 收尾写日志（>上游 1s 首帧延迟，确保 watchdog 已触发）

	page := waitForLog(t, store)
	if len(page.Logs) != 1 {
		t.Fatalf("应恰 1 条日志，实际 %d", len(page.Logs))
	}
	l := page.Logs[0]
	if l.Status != "failure" {
		t.Fatalf("客户端断开应记 failure，实际 %s", l.Status)
	}
	if l.ErrorMsg == nil || !strings.Contains(*l.ErrorMsg, "客户端断开") {
		t.Fatalf("错误应归类为「客户端断开」，实际 %v", l.ErrorMsg)
	}
	if l.RetryCount != 0 {
		t.Fatalf("客户端断开不应烧重试槽位（retry_count 应 0），实际 %d", l.RetryCount)
	}
	if atomic.LoadInt32(&upstreamHits) != 1 {
		t.Fatalf("上游应只收到 1 次请求（旧实现对每 Key 真实发起共 3 次），实际 %d", atomic.LoadInt32(&upstreamHits))
	}
}

// TestWhitelistSharedAliasRoutesToAllowedProvider A4（V-M4）回归：双供应商共享别名 shared
// （分别指向 a-real/b-real），Key 白名单=[b-real]。客户端请求 shared：原始名 shared 不在白名单 →
// 仅保留自身真实名在白名单内的候选（B），路由只在 B 选择 → 200 且 B 被调用、A 不被调用
// （旧实现只按全局首候选 a-real 归一匹配 → 合法调用被整单 403）。
func TestWhitelistSharedAliasRoutesToAllowedProvider(t *testing.T) {
	var aHits, bHits int32
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&aHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"from-a"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&bHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"from-b"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upB.Close()
	h, _, cleanup := newAuditStack(t, []auditProviderSpec{
		{id: "pa", name: "provA", baseURL: upA.URL, apiType: convert.FormatOpenAI, keys: 1, model: "a-real", aliases: []string{"shared"}},
		{id: "pb", name: "provB", baseURL: upB.URL, apiType: convert.FormatOpenAI, keys: 1, model: "b-real", aliases: []string{"shared"}},
	}, []string{"b-real"})
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	if code, body := fixCall(t, ts, "/v1/chat/completions", fmt.Sprintf(fixCCBody, `"shared"`)); code != 200 {
		t.Fatalf("共享别名经白名单允许的候选应 200（旧实现按首候选 a-real 归一匹配误 403），实际 %d: %s", code, body)
	}
	if atomic.LoadInt32(&aHits) != 0 || atomic.LoadInt32(&bHits) != 1 {
		t.Fatalf("应只调用 B（白名单允许），实际 A=%d B=%d", aHits, bHits)
	}
}

// TestWhitelistFiltersToEmptyReturns403 A4 互补：白名单匹配不到任何候选 → 过滤后为空 → 403，
// 且不调用任何供应商（过滤发生在路由之前）。
func TestWhitelistFiltersToEmptyReturns403(t *testing.T) {
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()
	h, _, cleanup := newAuditStack(t, []auditProviderSpec{
		{id: "pa", name: "provA", baseURL: up.URL, apiType: convert.FormatOpenAI, keys: 1, model: "a-real", aliases: []string{"shared"}},
		{id: "pb", name: "provB", baseURL: up.URL, apiType: convert.FormatOpenAI, keys: 1, model: "b-real", aliases: []string{"shared"}},
	}, []string{"c-real"}) // 不存在的真实名 → 过滤后为空
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	if code, _ := fixCall(t, ts, "/v1/chat/completions", fmt.Sprintf(fixCCBody, `"shared"`)); code != http.StatusForbidden {
		t.Fatalf("白名单匹配不到任何候选应 403，实际 %d", code)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("403 场景不应调用任何供应商，实际 hits=%d", hits)
	}
}

// TestTruncateUTF8Boundary C5 回归：截断点回退到最近的 UTF-8 字符边界，
// 避免中文等多字节字符被截半产生乱码；结果始终为合法 UTF-8。
func TestTruncateUTF8Boundary(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		n          int
		wantPrefix string // 期望前缀（截断时为截断后的原文部分，不截断时为原文本身）
	}{
		{"中文截在第二字节(回退到首字)", "中文abc", 4, "中"},
		{"两个中文截在第二字中间(回退到首字)", "中中", 4, "中"},
		{"恰好等于字节边界(不截断)", "中a", 4, "中a"},
		{"纯 ASCII 截断", "abcdefgh", 4, "abcd"},
		{"短于上限不截断", "中文abc", 100, "中文abc"},
		{"空串", "", 4, ""},
	}
	for _, c := range cases {
		got := truncate(c.in, c.n)
		if !strings.HasPrefix(got, c.wantPrefix) {
			t.Errorf("%s: 前缀应为 %q，实际 %q", c.name, c.wantPrefix, got)
			continue
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: 结果不是合法 UTF-8: %q", c.name, got)
		}
		// 截断时应带 ...(truncated) 标记；不截断时不应带
		if len(c.in) <= c.n {
			if strings.Contains(got, "...(truncated)") {
				t.Errorf("%s: 不应截断却带标记 %q", c.name, got)
			}
		} else {
			if !strings.HasSuffix(got, "...(truncated)") {
				t.Errorf("%s: 截断应带尾部标记，实际 %q", c.name, got)
			}
		}
	}
}
