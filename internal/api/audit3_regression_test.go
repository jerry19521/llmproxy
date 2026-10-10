package api

// v1.8.4 复查修复的回归测试（对应需求 12 版本更新记录 v1.8.4 小节）：
//   M9  fetch-models 表单模式 Key 上限 50 / M10 登录失败按 IP 滑窗限流 /
//   A4  apikey 更新锁内捕获更新结果 / A6 dashboard「今日」边界本地时区 RFC3339 /
//   A7  statistics dimension 白名单 / A8 custom 全未来区间 400 /
//   A9  脱敏 Key 恢复精确比对 / A10 测试请求 max_tokens=16 / A11 base_url 协议校验
// A2（供应商测试 10 分钟总预算）为 context.WithTimeout 时间类行为，单测无法压缩等待；
// 其 ctx 传递链路由 TestProviderTestMaxTokens16 经 mock 上游走完整 testOneKey 路径覆盖。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"llmproxy/internal/config"
	"llmproxy/internal/core"
	"llmproxy/internal/logsdb"
)

// ===== M9：fetch-models 表单模式 Key 数量上限 =====

// TestFetchModelsFormKeyLimit50 M9 回归：表单模式自动获取模型的 Key 数量上限。
// 旧实现无上限（L12 只给测试接口加了），8MB 请求体可塞入十余万条 Key 目标，
// 串行尝试（每条 60s 超时）可把请求挂起数天。
func TestFetchModelsFormKeyLimit50(t *testing.T) {
	s, _ := newAuditServer(t)
	do := func(n int) int {
		keys := make([]string, 0, n)
		for i := 0; i < n; i++ {
			// 空 Key：放行上限校验后在尝试循环内跳过，落「没有可用的 API Key」——零网络开销
			keys = append(keys, fmt.Sprintf(`{"id":"k%d","key":""}`, i))
		}
		body := `{"base_url":"https://upstream.invalid","api_type":"openai","api_keys":[` +
			strings.Join(keys, ",") + `]}`
		req := httptest.NewRequest("POST", "/api/providers/fetch-models", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleFetchModelsForm(rec, req)
		return rec.Code
	}
	if got := do(51); got != http.StatusBadRequest {
		t.Errorf("M9：51 条 Key 应 400，实际 %d", got)
	}
	if got := do(50); got != http.StatusBadGateway {
		t.Errorf("M9：50 条 Key 应通过上限检查（空 Key 落「没有可用的 API Key」502），实际 %d", got)
	}
}

// ===== M10：登录失败按 IP 滑窗限流 =====

// TestLoginThrottleIPBlock M10 回归：同一 IP 在窗口内失败达到上限后拒绝新尝试（429）。
// 旧实现登录接口对任意来源的失败尝试不做计数，配合默认监听 0.0.0.0 可被长期离线爆破。
// loginThrottle 为包级全局状态：本用例使用独有来源 IP，不与其他用例互相干扰。
func TestLoginThrottleIPBlock(t *testing.T) {
	s, _ := newAuditServer(t)
	const remote = "10.99.99.1:54321"
	attempt := func() int {
		req := httptest.NewRequest("POST", "/api/login",
			strings.NewReader(`{"username":"admin","password":"definitely-wrong-pass"}`))
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		s.handleLogin(rec, req)
		return rec.Code
	}
	for i := 0; i < loginFailMaxCount; i++ {
		if got := attempt(); got != http.StatusUnauthorized {
			t.Fatalf("M10：第 %d 次失败登录应 401，实际 %d", i+1, got)
		}
	}
	if got := attempt(); got != http.StatusTooManyRequests {
		t.Errorf("M10：窗口内失败 %d 次后新尝试应 429，实际 %d", loginFailMaxCount, got)
	}
}

// ===== A6：dashboard「今日」边界按服务器本地时区 =====

// TestDashboardTodayStartLocalTimezone A6 回归：todayStart 改为本地时区 RFC3339。
// 旧实现手工拼接无时区后缀的 YYYY-MMDD T00:00:00，绕过全局归一化口径，
// timestamp 存储格式调整时该处裸格式会静默错乱。
func TestDashboardTodayStartLocalTimezone(t *testing.T) {
	s, dir := newAuditServer(t)
	store, err := logsdb.Open(filepath.Join(dir, "a6.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s.SetLogStore(store)
	now := time.Now()
	write := func(id string, ts time.Time) {
		store.WriteAsync(&logsdb.CallLog{
			ID: id, Timestamp: ts.Format(time.RFC3339), APIKey: "sk-test", APIKeyID: "kid",
			Provider: "P", RequestedModel: "m", ActualModel: "m", Status: "success", DurationMs: 10,
		})
	}
	write("today-1", now)                  // 本地时区当前时刻 → 计入今日
	write("old-1", now.AddDate(0, 0, -30)) // 30 天前 → 只计入总量
	waitForCount(t, store, 2)

	req := httptest.NewRequest("GET", "/api/dashboard", nil)
	rec := httptest.NewRecorder()
	s.handleDashboard(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("A6：dashboard 应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var out struct {
		TodayCalls int `json:"today_calls"`
		TotalCalls int `json:"total_calls"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.TodayCalls != 1 || out.TotalCalls != 2 {
		t.Errorf("A6：今日边界错计——today=%d（应 1）、total=%d（应 2）；todayStart 未按本地时区 RFC3339 计算时跨时区部署会错计", out.TodayCalls, out.TotalCalls)
	}
}

// ===== A7：statistics dimension 白名单 =====

// TestStatisticsDimensionWhitelist A7 回归：非法 dimension 直接 400。
// 旧实现静默返回空对象，前端拼写错误与「区间内确实无数据」无法区分。
func TestStatisticsDimensionWhitelist(t *testing.T) {
	s, dir := newAuditServer(t)
	store, err := logsdb.Open(filepath.Join(dir, "a7.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s.SetLogStore(store)
	writeLogEntry(t, store, "a7-1")
	waitForCount(t, store, 1)
	do := func(dim string) int {
		u := "/api/statistics?range=7d"
		if dim != "" {
			u += "&dimension=" + dim
		}
		req := httptest.NewRequest("GET", u, nil)
		rec := httptest.NewRecorder()
		s.handleStatistics(rec, req)
		return rec.Code
	}
	if got := do("bogus"); got != http.StatusBadRequest {
		t.Errorf("A7：非法 dimension 应 400（旧实现静默返回空对象），实际 %d", got)
	}
	for _, dim := range []string{"", "timeline", "provider", "model", "apikey", "apikey_detail", "summary"} {
		if got := do(dim); got != http.StatusOK {
			t.Errorf("A7：合法 dimension=%q 应 200，实际 %d", dim, got)
		}
	}
}

// ===== A8：custom 全未来区间 400 =====

// TestStatisticsCustomFutureRangeRejected A8 回归：custom 区间完全落在未来时 400。
// 旧实现 end 被 clamp 到 now 后 start 仍在未来，形成空区间静默返回全零，用户误判服务故障。
func TestStatisticsCustomFutureRangeRejected(t *testing.T) {
	s, dir := newAuditServer(t)
	store, err := logsdb.Open(filepath.Join(dir, "a8.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s.SetLogStore(store)
	do := func(q string) int {
		req := httptest.NewRequest("GET", "/api/statistics"+q, nil)
		rec := httptest.NewRecorder()
		s.handleStatistics(rec, req)
		return rec.Code
	}
	if got := do("?range=custom&custom_start=2999-01-01&custom_end=2999-01-02"); got != http.StatusBadRequest {
		t.Errorf("A8：全未来区间应 400（旧实现静默返回全零），实际 %d", got)
	}
	if got := do("?range=custom&custom_start=2020-01-01&custom_end=2999-01-01"); got != http.StatusOK {
		t.Errorf("A8：跨当前时间的区间应 200（end 被 clamp 到 now），实际 %d", got)
	}
	// v1.8.3（F12）反向区间顺手回归
	if got := do("?range=custom&custom_start=2026-01-02&custom_end=2026-01-01"); got != http.StatusBadRequest {
		t.Errorf("A8：反向区间应 400（F12），实际 %d", got)
	}
}

// ===== A9：脱敏 Key 恢复改精确比对 =====

// TestMaskedRestoreByKeyIDExactMatch A9 回归：以「提交值 == MaskKey(现有 Key)」精确比对
// 取代 Contains("***") 内容特征。部分上游网关的自定义凭证可能字面含 ***——
// 旧实现会把用户输入的新 Key 误判为脱敏回显、静默恢复成旧值。
func TestMaskedRestoreByKeyIDExactMatch(t *testing.T) {
	realKey := "sk-live-***-credential" // 真实 Key 字面含 ***
	saved := []config.ProviderAPIKey{{ID: "k1", Key: realKey}}
	if full, ok := maskedRestoreByKeyID(core.MaskKey(realKey), "k1", saved); !ok || full != realKey {
		t.Errorf("A9：精确脱敏回显应恢复原值，got ok=%v full=%q", ok, full)
	}
	if _, ok := maskedRestoreByKeyID("sk-new-***-value99", "k1", saved); ok {
		t.Errorf("A9：字面含 *** 的新 Key 不应被误判为脱敏回显（旧实现会静默恢复成旧值）")
	}
	if _, ok := maskedRestoreByKeyID(core.MaskKey(realKey), "k2", saved); ok {
		t.Errorf("A9：id 不匹配时不应恢复")
	}
}

// ===== A10：测试请求 max_tokens=16（经 mock 上游走完整链路） =====

// TestProviderTestMaxTokens16 A10 回归：连通性测试请求 max_tokens 从 4096 降到 16。
// 测试目的是验证鉴权与端点可达，4096 允许上游生成全额 token 计费；
// 50 条 Key 串行测试时副作用 ×50。经 mock 上游走完整 testOneKey 链路
// （同时覆盖 A2 的 ctx 传递路径）。
func TestProviderTestMaxTokens16(t *testing.T) {
	var mu sync.Mutex
	var gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()

	s, _ := newAuditServer(t)
	body := fmt.Sprintf(`{"base_url":%q,"api_type":"openai","api_keys":[{"key":"sk-test-a10"}],"model":"gpt-test"}`, up.URL)
	req := httptest.NewRequest("POST", "/api/providers/test", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleProviderTest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("A10：mock 上游测试应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	gb := gotBody
	mu.Unlock()
	if !strings.Contains(gb, `"max_tokens":16`) {
		t.Errorf("A10：测试请求应 max_tokens=16（原 4096），上游实际收到：%s", gb)
	}
}

// ===== A11：test / fetch-models base_url 协议校验 =====

// TestBaseURLProtocolValidated A11 回归：两接口的 base_url 需以 http:// 或 https:// 开头。
// 旧实现非法前缀延迟到 client.Do 才以原始传输层错误失败，提示不友好且与 CRUD 表单口径不一致。
func TestBaseURLProtocolValidated(t *testing.T) {
	s, _ := newAuditServer(t)
	testDo := func(body string) int {
		req := httptest.NewRequest("POST", "/api/providers/test", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleProviderTest(rec, req)
		return rec.Code
	}
	fetchDo := func(body string) int {
		req := httptest.NewRequest("POST", "/api/providers/fetch-models", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleFetchModelsForm(rec, req)
		return rec.Code
	}
	if got := testDo(`{"base_url":"gopher://x","api_type":"openai","api_keys":[{"key":"sk-x"}],"model":"m"}`); got != http.StatusBadRequest {
		t.Errorf("A11：test 接口非 http(s) 协议应 400，实际 %d", got)
	}
	if got := fetchDo(`{"base_url":"ftp://x","api_type":"openai","api_keys":[{"key":"sk-x"}]}`); got != http.StatusBadRequest {
		t.Errorf("A11：fetch-models 非 http(s) 协议应 400，实际 %d", got)
	}
	// https 前缀应放行协议校验：空 Key 落「没有可用的 API Key」502（而非 400），零网络开销
	if got := fetchDo(`{"base_url":"https://x.example","api_type":"openai","api_keys":[]}`); got == http.StatusBadRequest {
		t.Errorf("A11：https 前缀应放行协议校验，实际 %d", got)
	}
}

// ===== A4：apikey 更新锁内捕获更新结果 =====

// TestAPIKeyUpdateConcurrentWithDelete A4 回归：更新接口的响应取自 Mutate 锁内捕获的
// updatedKey。旧实现在响应阶段再次 FindProxyKeyByID——并发 DELETE 同一 Key 落在窗口内时
// 返回 nil，toKeyView 解引用空指针 panic。panic 在并发窗口内偶发、无法确定性注入，
// 本用例以并发压测守护不再回归（任何一次 panic 都会直接炸掉测试进程）。
func TestAPIKeyUpdateConcurrentWithDelete(t *testing.T) {
	s, _ := newAuditServer(t)
	// 初始 Key
	create := func() string {
		body := fmt.Sprintf(`{"name":"a4","key":"sk-%s"}`, strings.Repeat("b", 32))
		req := httptest.NewRequest("POST", "/api/apikeys", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleAPIKeyCreate(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("A4：创建应 200，实际 %d：%s", rec.Code, rec.Body.String())
		}
		var out struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.ID
	}
	id := create()

	const rounds = 200
	var wg sync.WaitGroup
	allowed := map[int]bool{http.StatusOK: true, http.StatusBadRequest: true, http.StatusNotFound: true}
	recordCode := func(code int, where string) {
		if !allowed[code] {
			t.Errorf("A4：%s 出现异常状态码 %d", where, code)
		}
	}
	// 并发更新：同一 Key 反复改名（key 传脱敏回显走保留原值路径）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			req := httptest.NewRequest("PUT", "/api/apikeys/"+id,
				strings.NewReader(fmt.Sprintf(`{"name":"a4-%d"}`, i)))
			req.SetPathValue("id", id)
			rec := httptest.NewRecorder()
			s.handleAPIKeyUpdate(rec, req)
			recordCode(rec.Code, "update")
		}
	}()
	// 并发删建：同 id 反复删除（不存在时 404 属预期）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			req := httptest.NewRequest("DELETE", "/api/apikeys/"+id, nil)
			req.SetPathValue("id", id)
			rec := httptest.NewRecorder()
			s.handleAPIKeyDelete(rec, req)
			if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
				t.Errorf("A4：delete 出现异常状态码 %d", rec.Code)
			}
		}
	}()
	wg.Wait()
}
