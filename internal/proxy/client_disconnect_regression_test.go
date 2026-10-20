package proxy

// 回归测试（proxy 侧）：
//   客户端断开后非流式路径不再继续换 Key/供应商烧上游配额
//   Key 数 >3 的供应商失败后正常进入冷却池（按"实际尝试且全部失败"计一轮失败）
//   403 模型白名单越权拒绝写入调用日志
//   同格式流式写失败时 client_resp_body 只记已送达字节（与 sent_bytes 口径一致）

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llmproxy/internal/convert"
	"llmproxy/internal/core"
)

// TestNonStreamClientDisconnectStopsRetry 非流式请求客户端断开后，
// 第一次上游失败即终止重试，记「客户端断开」。
func TestNonStreamClientDisconnectStopsRetry(t *testing.T) {
	var upstreamHits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		time.Sleep(150 * time.Millisecond) // 确保客户端 cancel 先于上游失败返回
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer up.Close()
	h, store, cleanup := newMultiProviderStack(t, []providerSpec{{
		id: "p1", name: "up500", baseURL: up.URL, apiType: convert.FormatOpenAI, keys: 3, model: "testmodel",
	}}, nil)
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"testmodel","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-proxy-e2e-key")
	go func() {
		resp, err := (&http.Client{}).Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	time.Sleep(50 * time.Millisecond)  // 等请求进入代理处理
	cancel()                           // 客户端断开
	time.Sleep(600 * time.Millisecond) // 等第一次上游失败返回 + 收尾写日志

	page := waitForLog(t, store)
	if len(page.Logs) != 1 {
		t.Fatalf("应恰 1 条日志，实际 %d", len(page.Logs))
	}
	l := page.Logs[0]
	if l.Status != "failure" {
		t.Fatalf("应记 failure，实际 %s", l.Status)
	}
	if l.ErrorMsg == nil || !strings.Contains(*l.ErrorMsg, "客户端断开") {
		t.Fatalf("错误应归类「客户端断开」，实际 %v", l.ErrorMsg)
	}
	if n := atomic.LoadInt32(&upstreamHits); n != 1 {
		t.Fatalf("客户端断开后第一次上游失败即应终止，上游应只收到 1 次请求（烧满 3 Key），实际 %d", n)
	}
}

// TestMultiKeyProviderCooldownsAfterCappedFailures 配置 4 条 Key 的供应商
// （maxAttempts=3）全部失败一轮后应进入冷却池——按"实际尝试且全部失败"即计一轮失败，
// RecordFailure 在浅尝路径上也须生效，冷却熔断对多 Key 供应商不应静默失效。
func TestMultiKeyProviderCooldownsAfterCappedFailures(t *testing.T) {
	var upstreamHits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer up.Close()
	h, _, cleanup := newMultiProviderStack(t, []providerSpec{{
		id: "p1", name: "manykeys", baseURL: up.URL, apiType: convert.FormatOpenAI, keys: 4, model: "testmodel",
	}}, nil)
	defer cleanup()
	// 开启冷却：连续 1 轮失败即入池
	if err := h.State.Mutate(func(ns *core.Snapshot) {
		ns.CooldownEnabled = true
		ns.CooldownSeconds = 60
		ns.CooldownRetryCount = 1
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	// 第一轮：maxAttempts=3 次上游请求全失败（4 Key 只浅尝 3 条）
	if code, body := proxyCall(t, ts, "/v1/chat/completions", fmt.Sprintf(ccReqBody, `"testmodel"`)); code != http.StatusServiceUnavailable {
		t.Fatalf("第一轮全失败应 503，实际 %d: %s", code, body)
	}
	if n := atomic.LoadInt32(&upstreamHits); n != 3 {
		t.Fatalf("第一轮应试 3 条 Key（上限），实际 %d", n)
	}
	// 第二轮：供应商应已在冷却池中——请求直接 503 cooling down，不再发上游请求
	code, body := proxyCall(t, ts, "/v1/chat/completions", fmt.Sprintf(ccReqBody, `"testmodel"`))
	if !strings.Contains(body, "cooling down") {
		t.Fatalf("第二轮应因冷却直接 503 cooling down（熔断失效继续发上游请求），实际 %d: %s", code, body)
	}
	if n := atomic.LoadInt32(&upstreamHits); n != 3 {
		t.Fatalf("冷却中不应再发上游请求，实际 %d", n)
	}
}

// TestWhitelist403Logged 模型白名单拒绝（403）应写入调用日志，
// 越权探测行为可审计。
func TestWhitelist403Logged(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("403 场景不应调用上游")
	}))
	defer up.Close()
	h, store, cleanup := newMultiProviderStack(t, []providerSpec{
		{id: "pa", name: "provA", baseURL: up.URL, apiType: convert.FormatOpenAI, keys: 1, model: "a-real", aliases: []string{"shared"}},
	}, []string{"b-real"}) // 请求 shared，白名单只允许 b-real → 403
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	if code, _ := proxyCall(t, ts, "/v1/chat/completions", fmt.Sprintf(ccReqBody, `"shared"`)); code != http.StatusForbidden {
		t.Fatalf("白名单不匹配应 403，实际 %d", code)
	}
	page := waitForLog(t, store)
	if len(page.Logs) != 1 {
		t.Fatalf("403 越权拒绝应写 1 条日志（无痕），实际 %d", len(page.Logs))
	}
	// 列表查询不含大字段（性能优化设计），用单条详情接口取全字段
	l, err := store.Get(page.Logs[0].ID)
	if err != nil {
		t.Fatalf("读取日志详情失败: %v", err)
	}
	if l.Status != "failure" {
		t.Fatalf("应记 failure，实际 %s", l.Status)
	}
	if l.ErrorMsg == nil || !strings.Contains(*l.ErrorMsg, "model not allowed") {
		t.Fatalf("错误消息应含 model not allowed，实际 %v", l.ErrorMsg)
	}
	if l.ClientRespStatus != http.StatusForbidden {
		t.Fatalf("客户端响应状态应为 403，实际 %d", l.ClientRespStatus)
	}
	if l.ReqBody == "" {
		t.Fatalf("日志应含客户端请求体（审计需要）")
	}
}

// ===== passthroughStream clientBytes 口径（单元级直接调用） =====

// seqChunkReader 每次 Read 返回一个预置片段（模拟 SSE 分块到达）。
type seqChunkReader struct {
	parts [][]byte
	idx   int
}

func (r *seqChunkReader) Read(p []byte) (int, error) {
	if r.idx >= len(r.parts) {
		return 0, io.EOF
	}
	n := copy(p, r.parts[r.idx])
	r.idx++
	return n, nil
}

// failAtWriter 第 failAt 次 Write 起返回错误（模拟客户端断开）。
type failAtWriter struct {
	header  http.Header
	buf     bytes.Buffer
	writes  int
	failAt  int
	failErr error
}

func (w *failAtWriter) Header() http.Header { return w.header }
func (w *failAtWriter) WriteHeader(int)     {}
func (w *failAtWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes >= w.failAt {
		return 0, w.failErr
	}
	w.buf.Write(p)
	return len(p), nil
}

func TestPassthroughClientBytesExcludesUndelivered(t *testing.T) {
	part1 := []byte("data: {\"a\":1}\n\n")
	part2 := []byte("data: {\"b\":2}\n\n")
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(&seqChunkReader{parts: [][]byte{part1, part2}}),
	}
	w := &failAtWriter{header: http.Header{}, failAt: 2, failErr: errors.New("wsasend: broken pipe")}
	att := &streamAttempt{result: &upstreamResult{}}

	passthroughStream(w, resp, att, time.Now())

	if att.err == "" || !strings.Contains(att.err, "写入客户端失败") {
		t.Fatalf("应记录写失败，实际 err=%q", att.err)
	}
	// 上游侧副本保留完整读取字节（两块）
	if !bytes.Equal(att.result.RespBody, append(append([]byte{}, part1...), part2...)) {
		t.Fatalf("resp_body 应保留完整上游字节（含未送达尾块），实际 %q", att.result.RespBody)
	}
	// clientBytes 只含成功送达的第一块
	if !bytes.Equal(att.clientBytes, part1) {
		t.Fatalf("client_resp_body 应只含已送达字节（含未送达尾块），实际 %q", att.clientBytes)
	}
	if att.sentBytes != int64(len(part1)) {
		t.Fatalf("sent_bytes 应为第一块长度，实际 %d", att.sentBytes)
	}
	if int64(len(att.clientBytes)) > att.sentBytes && len(att.clientBytes) > 0 {
		t.Fatalf("client_resp_body 与 sent_bytes 口径应一致")
	}
}
