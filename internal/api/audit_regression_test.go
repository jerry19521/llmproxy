package api

// v1.8.3 复查修复的回归测试（对应需求 12 版本更新记录 v1.8.3 小节）：
//   M2  清空日志 store 锁内捕获 / L1 时间参数 400 / L3 Key 唯一性临界区复查 /
//   L7 aliases 非法类型报错 / L8 短 Key 脱敏 / L10 token 签发用户校验 /
//   L11 truncateStr UTF-8 边界 / L12 测试 Key 数量上限 / F1 编辑页自动获取脱敏 Key 恢复 /
//   F12 统计自定义区间反向 400

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"llmproxy/internal/auth"
	"llmproxy/internal/config"
	"llmproxy/internal/core"
	"llmproxy/internal/logsdb"
)

// newAuditServer 构造测试用管理接口（独立临时目录，互不干扰）。
func newAuditServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	m := config.NewManager(filepath.Join(dir, "cfg.yml"))
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	state := core.NewState(m)
	t.Cleanup(state.Stop)
	return &Server{Mgr: m, State: state, Listen: "127.0.0.1:0"}, dir
}

func writeLogEntry(t *testing.T, s *logsdb.LogStore, id string) {
	t.Helper()
	s.WriteAsync(&logsdb.CallLog{
		ID: id, Timestamp: logsdb.NowRFC3339(),
		APIKey: "sk-test", APIKeyID: "kid", Provider: "P",
		RequestedModel: "m", ActualModel: "m", Status: "success", DurationMs: 10,
	})
}

func waitForCount(t *testing.T, s *logsdb.LogStore, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n, err := s.Count(); err == nil && n >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	n, _ := s.Count()
	t.Fatalf("等待日志落盘超时：want=%d got=%d", want, n)
}

// TestLogsListInvalidTimeParamRejected L1 回归：无法解析的时间参数返回 400，
// 不再原样透传进 SQL 字符串比较（旧实现实测 start_time=abc 会过滤掉全部日志、
// end_time=abc 等于没过滤，均静默无提示）。
func TestLogsListInvalidTimeParamRejected(t *testing.T) {
	s, dir := newAuditServer(t)
	store, err := logsdb.Open(filepath.Join(dir, "l1.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s.SetLogStore(store)
	writeLogEntry(t, store, "id-1")
	waitForCount(t, store, 1)

	do := func(query string) int {
		req := httptest.NewRequest("GET", "/api/logs"+query, nil)
		rec := httptest.NewRecorder()
		s.handleLogsList(rec, req)
		return rec.Code
	}
	if got := do("?start_time=abc"); got != 400 {
		t.Errorf("start_time=abc 应 400，实际 %d", got)
	}
	if got := do("?end_time=abc"); got != 400 {
		t.Errorf("end_time=abc 应 400，实际 %d", got)
	}
	if got := do("?start_time=2026-13-99"); got != 400 {
		t.Errorf("非法日期应 400，实际 %d", got)
	}
	if got := do("?start_time=2020-01-01T00:00:00"); got != 200 {
		t.Errorf("合法时间应 200，实际 %d", got)
	}
	if got := do(""); got != 200 {
		t.Errorf("无时间参数应 200，实际 %d", got)
	}
}

// TestLogsClearConcurrentAfterSwap M2 回归：清空日志的 store 在锁内捕获。
// 并发双清空（OnLogRecreate 模拟 main 的重建+swap）下两个请求均应成功，
// 且结束后管理端持有的 store 仍可正常查询（无已关闭引用残留）。
func TestLogsClearConcurrentAfterSwap(t *testing.T) {
	s, dir := newAuditServer(t)
	store, err := logsdb.Open(filepath.Join(dir, "m2.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetLogStore(store)
	for i := 0; i < 3; i++ {
		writeLogEntry(t, store, fmt.Sprintf("id-%d", i))
	}
	waitForCount(t, store, 3)

	// 模拟 main 注入的 OnLogRecreate：关旧库删文件重建 + 同步替换 api/proxy 引用
	var mu sync.Mutex
	s.OnLogRecreate = func() error {
		mu.Lock()
		defer mu.Unlock()
		ns, err := s.LogStore().Recreate()
		if err != nil {
			return err
		}
		s.SetLogStore(ns)
		return nil
	}

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest("DELETE", "/api/logs", nil)
			rec := httptest.NewRecorder()
			s.handleLogsClear(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Errorf("并发清空请求 %d 应 200，实际 %d", i, c)
		}
	}
	if cur := s.LogStore(); cur == nil {
		t.Fatal("清空后管理端 store 引用不应为 nil")
	} else if _, err := cur.Query(logsdb.LogQuery{}); err != nil {
		t.Errorf("清空后对当前 store 查询应成功（不应残留已关闭引用），实际: %v", err)
	} else {
		_ = cur.Close() // 关闭重建后的新库，避免测试临时目录清理时文件被占用
	}
}

// TestAPIKeyCreateDuplicateRejected L3 回归：同 Key 重复创建被 400 拒绝；
// 并发提交同 Key 时最终配置恰好一条（临界区内复查，消除锁外预检 TOCTOU 窗口）。
func TestAPIKeyCreateDuplicateRejected(t *testing.T) {
	s, _ := newAuditServer(t)
	key := "sk-abcdefghijklmnopqrstuvwxyz012345" // sk- + 32 位字母数字（a-z 26 + 0-5 6 = 32）
	doCreate := func() (int, string) {
		body := fmt.Sprintf(`{"name":"k","key":"%s","ip_whitelist":["*"],"enabled":true,"models":[]}`, key)
		req := httptest.NewRequest("POST", "/api/apikeys", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleAPIKeyCreate(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := doCreate(); code != 200 {
		t.Fatalf("首次创建应 200，实际 %d body=%s", code, body)
	}
	if code, body := doCreate(); code != 400 || !strings.Contains(body, "已存在") {
		t.Errorf("重复创建应 400 已存在，实际 %d body=%s", code, body)
	}
	// 并发 4 个同 Key 请求：最终配置必须恰好一条（无论交错顺序）
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			doCreate()
		}()
	}
	wg.Wait()
	n := 0
	for _, k := range s.State.Snap().APIKeys {
		if k.Key == key {
			n++
		}
	}
	if n != 1 {
		t.Errorf("并发重复提交后配置中应恰好 1 条该 Key，实际 %d 条", n)
	}
}

// TestStatisticsCustomRangeReversedRejected F12 后端回归：自定义区间 start > end 返回 400，
// 不再静默落入空区间返回全零数据。
func TestStatisticsCustomRangeReversedRejected(t *testing.T) {
	s, dir := newAuditServer(t)
	store, err := logsdb.Open(filepath.Join(dir, "f12.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s.SetLogStore(store)
	req := httptest.NewRequest("GET", "/api/statistics?range=custom&custom_start=2026-10-05&custom_end=2026-10-01", nil)
	rec := httptest.NewRecorder()
	s.handleStatistics(rec, req)
	if rec.Code != 400 {
		t.Errorf("反向自定义区间应 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestRequireAuthUsernameMismatchRejected L10 回归：token 签发用户与当前 admin.username
// 不一致时 401（修改用户名后旧 token 立即失效，而非持续有效到自然过期）。
func TestRequireAuthUsernameMismatchRejected(t *testing.T) {
	s, _ := newAuditServer(t)
	cfg := s.Mgr.Cfg()
	token, _ := auth.IssueToken(cfg.JWT.Secret, cfg.Admin.Username, cfg.Admin.Password, cfg.JWT.ExpireHours)
	// 修改配置中的用户名（密码不变）
	if err := s.Mgr.Update(func(c *config.Config) { c.Admin.Username = "newadmin" }); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/settings", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.requireAuth(s.handleSettingsGet)(rec, req)
	if rec.Code != 401 {
		t.Errorf("用户名变更后旧 token 应 401，实际 %d", rec.Code)
	}
}

// TestFlexAliasesInvalidTypeRejected L7 回归：aliases 传数字/对象不再静默归为空数组，
// 而是报错（请求体解码阶段 400）。
func TestFlexAliasesInvalidTypeRejected(t *testing.T) {
	var a FlexAliases
	if err := json.Unmarshal([]byte(`123`), &a); err == nil {
		t.Error("aliases 传数字应报错，实际被静默接受")
	}
	if err := json.Unmarshal([]byte(`{"x":1}`), &a); err == nil {
		t.Error("aliases 传对象应报错，实际被静默接受")
	}
	if err := json.Unmarshal([]byte(`"a,b"`), &a); err != nil || len(a) != 2 {
		t.Errorf("字符串形式应正常解析为 2 项，err=%v len=%d", err, len(a))
	}
	if err := json.Unmarshal([]byte(`["a","b"]`), &a); err != nil || len(a) != 2 {
		t.Errorf("数组形式应正常解析为 2 项，err=%v len=%d", err, len(a))
	}
}

// TestTruncateStrUTF8Boundary L11 回归：截断点回退到 UTF-8 字符边界，
// 中文错误消息不再截出乱码尾字节（与 proxy 包 truncate 的 C5 修复同口径）。
func TestTruncateStrUTF8Boundary(t *testing.T) {
	s := "你好世界你好世界你好世界"       // 每字 3 字节
	got := truncateStr(s, 10) // 10 字节落在第 4 个字符中间
	if !utf8.ValidString(got) {
		t.Errorf("截断结果应为合法 UTF-8（中文不截半），实际 %q", got)
	}
	// 截断点应在 9 字节边界（第 3 个字符结束后）
	if got != "你好世..." {
		t.Errorf("截断结果 = %q, want %q", got, "你好世...")
	}
	if truncateStr("abc", 10) != "abc" {
		t.Error("未超限应原样返回")
	}
}

// TestMaskKeyShortPrefixOnly L8 回归：短 Key（7-10 字符）只保留前 3 位，
// 不再按前 3+后 3 泄露 6/7 字符。
func TestMaskKeyShortPrefixOnly(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"abcdef", "***"},                 // 6 字符：全遮蔽
		{"abcdefg", "abc***"},             // 7 字符：旧实现泄露 6/7 字符
		{"abcdefghij", "abc***"},          // 10 字符
		{"abcdefghijk", "abc***ijk"},      // 11 字符：前 3 + 后 3
		{"sk-proxy-e2e-key", "sk-***key"}, // 常规长度：前 3 + 后 3
	}
	for _, c := range cases {
		if got := core.MaskKey(c.in); got != c.want {
			t.Errorf("MaskKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestProviderTestKeyCountLimit L12 回归：单次测试 Key 数量超过 50 返回 400，
// 不再无上限串行执行（每 Key 120s 超时可把请求挂数分钟）。
func TestProviderTestKeyCountLimit(t *testing.T) {
	s, _ := newAuditServer(t)
	var sb strings.Builder
	sb.WriteString(`{"base_url":"http://127.0.0.1:1","api_type":"openai","model":"m","api_keys":[`)
	for i := 0; i < 51; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"id":"k%d","key":"sk-x%d"}`, i, i)
	}
	sb.WriteString(`]}`)
	req := httptest.NewRequest("POST", "/api/providers/test", strings.NewReader(sb.String()))
	rec := httptest.NewRecorder()
	s.handleProviderTest(rec, req)
	if rec.Code != 400 {
		t.Errorf("51 条 Key 测试应 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestFetchModelsFormRestoresMaskedKey F1 后端回归：POST 表单模式自动获取模型时，
// 编辑页的脱敏 Key（含 ***）按 provider_id + key id 恢复完整值参与请求。
func TestFetchModelsFormRestoresMaskedKey(t *testing.T) {
	s, _ := newAuditServer(t)
	// mock 上游 /models：记录收到的 Authorization
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"mock-model"}]}`))
	}))
	defer up.Close()

	const fullKey = "sk-fullkey-1234567890abcdef"
	pid := "p-audit-f1"
	if err := s.State.Mutate(func(ns *core.Snapshot) {
		ns.Providers = append(ns.Providers, &config.Provider{
			ID: pid, Name: "P", BaseURL: up.URL, APIType: "openai", Enabled: true,
			APIKeys: []config.ProviderAPIKey{{ID: "k1", Key: fullKey}},
		})
	}); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"provider_id":%q,"base_url":%q,"api_type":"openai","api_keys":[{"id":"k1","key":%q}],"headers":[]}`,
		pid, up.URL, core.MaskKey(fullKey))
	req := httptest.NewRequest("POST", "/api/providers/fetch-models", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleFetchModelsForm(rec, req)
	if rec.Code != 200 {
		t.Fatalf("自动获取应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "mock-model") {
		t.Errorf("响应应含 mock-model: %s", rec.Body.String())
	}
	if gotAuth != "Bearer "+fullKey {
		t.Errorf("上游应收到恢复后的完整 Key，实际 Authorization=%q", gotAuth)
	}
}
