package proxy

// v1.8.4 第三轮全量审查回归测试（proxy 侧）：
//   M2  客户端断开后非流式路径不再继续换 Key/供应商烧上游配额
//   M3  Key 数 >3 的供应商失败后正常进入冷却池（旧实现 allKeysTried 恒 false，熔断静默失效）
//   P4  403 模型白名单越权拒绝写入调用日志（旧实现完全无痕）
//   P6  同格式流式写失败时 client_resp_body 只记已送达字节（与 sent_bytes 口径一致）

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

// TestAudit3NonStreamClientDisconnectStopsRetry M2 回归：非流式请求客户端断开后，
// 第一次上游失败即终止重试（旧实现继续烧满 3 Key），记「客户端断开」。
func TestAudit3NonStreamClientDisconnectStopsRetry(t *testing.T) {
	var upstreamHits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		time.Sleep(150 * time.Millisecond) // 确保客户端 cancel 先于上游失败返回
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer up.Close()
	h, store, cleanup := newAuditStack(t, []auditProviderSpec{{
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
		t.Fatalf("M2：应恰 1 条日志，实际 %d", len(page.Logs))
	}
	l := page.Logs[0]
	if l.Status != "failure" {
		t.Fatalf("M2：应记 failure，实际 %s", l.Status)
	}
	if l.ErrorMsg == nil || !strings.Contains(*l.ErrorMsg, "客户端断开") {
		t.Fatalf("M2：错误应归类「客户端断开」，实际 %v", l.ErrorMsg)
	}
	if n := atomic.LoadInt32(&upstreamHits); n != 1 {
		t.Fatalf("M2：客户端断开后第一次上游失败即应终止，上游应只收到 1 次请求（旧实现烧满 3 Key），实际 %d", n)
	}
}

// TestAudit3MultiKeyProviderCooldownsAfterCappedFailures M3 回归：配置 4 条 Key 的供应商
// （maxAttempts=3）全部失败一轮后应进入冷却池——旧实现 allKeysTried 恒 false，
// RecordFailure 永不执行，冷却熔断对该供应商静默失效。
func TestAudit3MultiKeyProviderCooldownsAfterCappedFailures(t *testing.T) {
	var upstreamHits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer up.Close()
	h, _, cleanup := newAuditStack(t, []auditProviderSpec{{
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
	if code, body := fixCall(t, ts, "/v1/chat/completions", fmt.Sprintf(fixCCBody, `"testmodel"`)); code != http.StatusServiceUnavailable {
		t.Fatalf("M3：第一轮全失败应 503，实际 %d: %s", code, body)
	}
	if n := atomic.LoadInt32(&upstreamHits); n != 3 {
		t.Fatalf("M3：第一轮应试 3 条 Key（上限），实际 %d", n)
	}
	// 第二轮：供应商应已在冷却池中——请求直接 503 cooling down，不再发上游请求
	code, body := fixCall(t, ts, "/v1/chat/completions", fmt.Sprintf(fixCCBody, `"testmodel"`))
	if !strings.Contains(body, "cooling down") {
		t.Fatalf("M3：第二轮应因冷却直接 503 cooling down（旧实现熔断失效继续发上游请求），实际 %d: %s", code, body)
	}
	if n := atomic.LoadInt32(&upstreamHits); n != 3 {
		t.Fatalf("M3：冷却中不应再发上游请求，实际 %d", n)
	}
}

// TestAudit3Whitelist403Logged P4 回归：模型白名单拒绝（403）应写入调用日志，
// 越权探测行为可审计（旧实现完全无痕）。
func TestAudit3Whitelist403Logged(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("403 场景不应调用上游")
	}))
	defer up.Close()
	h, store, cleanup := newAuditStack(t, []auditProviderSpec{
		{id: "pa", name: "provA", baseURL: up.URL, apiType: convert.FormatOpenAI, keys: 1, model: "a-real", aliases: []string{"shared"}},
	}, []string{"b-real"}) // 请求 shared，白名单只允许 b-real → 403
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(h.ServeV1))
	defer ts.Close()

	if code, _ := fixCall(t, ts, "/v1/chat/completions", fmt.Sprintf(fixCCBody, `"shared"`)); code != http.StatusForbidden {
		t.Fatalf("P4：白名单不匹配应 403，实际 %d", code)
	}
	page := waitForLog(t, store)
	if len(page.Logs) != 1 {
		t.Fatalf("P4：403 越权拒绝应写 1 条日志（旧实现无痕），实际 %d", len(page.Logs))
	}
	// 列表查询不含大字段（性能优化设计），用单条详情接口取全字段
	l, err := store.Get(page.Logs[0].ID)
	if err != nil {
		t.Fatalf("P4：读取日志详情失败: %v", err)
	}
	if l.Status != "failure" {
		t.Fatalf("P4：应记 failure，实际 %s", l.Status)
	}
	if l.ErrorMsg == nil || !strings.Contains(*l.ErrorMsg, "model not allowed") {
		t.Fatalf("P4：错误消息应含 model not allowed，实际 %v", l.ErrorMsg)
	}
	if l.ClientRespStatus != http.StatusForbidden {
		t.Fatalf("P4：客户端响应状态应为 403，实际 %d", l.ClientRespStatus)
	}
	if l.ReqBody == "" {
		t.Fatalf("P4：日志应含客户端请求体（审计需要）")
	}
}

// ===== P6：passthroughStream clientBytes 口径（单元级直接调用） =====

// audit3SeqReader 每次 Read 返回一个预置片段（模拟 SSE 分块到达）。
type audit3SeqReader struct {
	parts [][]byte
	idx   int
}

func (r *audit3SeqReader) Read(p []byte) (int, error) {
	if r.idx >= len(r.parts) {
		return 0, io.EOF
	}
	n := copy(p, r.parts[r.idx])
	r.idx++
	return n, nil
}

// audit3FailWriter 第 failAt 次 Write 起返回错误（模拟客户端断开）。
type audit3FailWriter struct {
	header  http.Header
	buf     bytes.Buffer
	writes  int
	failAt  int
	failErr error
}

func (w *audit3FailWriter) Header() http.Header { return w.header }
func (w *audit3FailWriter) WriteHeader(int)     {}
func (w *audit3FailWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes >= w.failAt {
		return 0, w.failErr
	}
	w.buf.Write(p)
	return len(p), nil
}

func TestAudit3PassthroughClientBytesExcludesUndelivered(t *testing.T) {
	part1 := []byte("data: {\"a\":1}\n\n")
	part2 := []byte("data: {\"b\":2}\n\n")
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(&audit3SeqReader{parts: [][]byte{part1, part2}}),
	}
	w := &audit3FailWriter{header: http.Header{}, failAt: 2, failErr: errors.New("wsasend: broken pipe")}
	att := &streamAttempt{result: &upstreamResult{}}

	passthroughStream(w, resp, att, time.Now())

	if att.err == "" || !strings.Contains(att.err, "写入客户端失败") {
		t.Fatalf("P6：应记录写失败，实际 err=%q", att.err)
	}
	// 上游侧副本保留完整读取字节（两块）
	if !bytes.Equal(att.result.RespBody, append(append([]byte{}, part1...), part2...)) {
		t.Fatalf("P6：resp_body 应保留完整上游字节（含未送达尾块），实际 %q", att.result.RespBody)
	}
	// clientBytes 只含成功送达的第一块
	if !bytes.Equal(att.clientBytes, part1) {
		t.Fatalf("P6：client_resp_body 应只含已送达字节（旧实现含未送达尾块），实际 %q", att.clientBytes)
	}
	if att.sentBytes != int64(len(part1)) {
		t.Fatalf("P6：sent_bytes 应为第一块长度，实际 %d", att.sentBytes)
	}
	if int64(len(att.clientBytes)) > att.sentBytes && len(att.clientBytes) > 0 {
		t.Fatalf("P6：client_resp_body 与 sent_bytes 口径应一致")
	}
}
