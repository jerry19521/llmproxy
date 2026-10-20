package api

// 回归测试：
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
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"llmproxy/internal/auth"
	"llmproxy/internal/config"
	"llmproxy/internal/core"
	"llmproxy/internal/logsdb"
)

// newTestServer 构造测试用管理接口（独立临时目录，互不干扰）。
func newTestServer(t *testing.T) (*Server, string) {
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

// TestLogsListInvalidTimeParamRejected L1 无法解析的时间参数返回 400，
// 不再原样透传进 SQL 字符串比较（实测 start_time=abc 会过滤掉全部日志、
// end_time=abc 等于没过滤，均静默无提示）。
func TestLogsListInvalidTimeParamRejected(t *testing.T) {
	s, dir := newTestServer(t)
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

// TestLogsClearConcurrentAfterSwap M2 清空日志的 store 在锁内捕获。
// 并发双清空（OnLogRecreate 模拟 main 的重建+swap）下两个请求均应成功，
// 且结束后管理端持有的 store 仍可正常查询（无已关闭引用残留）。
func TestLogsClearConcurrentAfterSwap(t *testing.T) {
	s, dir := newTestServer(t)
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

// TestAPIKeyCreateDuplicateRejected L3 同 Key 重复创建被 400 拒绝；
// 并发提交同 Key 时最终配置恰好一条（临界区内复查，消除锁外预检 TOCTOU 窗口）。
func TestAPIKeyCreateDuplicateRejected(t *testing.T) {
	s, _ := newTestServer(t)
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

// TestStatisticsCustomRangeReversedRejected F12 自定义区间 start > end 返回 400，
// 不再静默落入空区间返回全零数据。
func TestStatisticsCustomRangeReversedRejected(t *testing.T) {
	s, dir := newTestServer(t)
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

// TestRequireAuthUsernameMismatchRejected L10 token 签发用户与当前 admin.username
// 不一致时 401（修改用户名后旧 token 立即失效，而非持续有效到自然过期）。
func TestRequireAuthUsernameMismatchRejected(t *testing.T) {
	s, _ := newTestServer(t)
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

// TestFlexAliasesInvalidTypeRejected L7 aliases 传数字/对象不再静默归为空数组，
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

// TestTruncateStrUTF8Boundary L11 截断点回退到 UTF-8 字符边界，
// 中文错误消息不再截出乱码尾字节（同口径）。
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

// TestMaskKeyShortPrefixOnly L8 短 Key（7-10 字符）只保留前 3 位，
// 不再按前 3+后 3 泄露 6/7 字符。
func TestMaskKeyShortPrefixOnly(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"abcdef", "***"},                 // 6 字符：全遮蔽
		{"abcdefg", "abc***"},             // 7 字符：泄露 6/7 字符
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

// TestProviderTestKeyCountLimit L12 单次测试 Key 数量超过 50 返回 400，
// 不再无上限串行执行（每 Key 120s 超时可把请求挂数分钟）。
func TestProviderTestKeyCountLimit(t *testing.T) {
	s, _ := newTestServer(t)
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

// TestFetchModelsFormRestoresMaskedKey F1 POST 表单模式自动获取模型时，
// 编辑页的脱敏 Key（含 ***）按 provider_id + key id 恢复完整值参与请求。
func TestFetchModelsFormRestoresMaskedKey(t *testing.T) {
	s, _ := newTestServer(t)
	// mock 上游 /models：记录收到的 Authorization
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"mock-model"}]}`))
	}))
	defer up.Close()

	const fullKey = "sk-fullkey-1234567890abcdef"
	pid := "p-f1"
	if err := s.State.Mutate(func(ns *core.Snapshot) {
		ns.Providers = append(ns.Providers, &config.Provider{
			ID: pid, Name: "P", BaseURL: up.URL, APIType: "openai", Enabled: true,
			APIKeys: []config.ProviderAPIKey{{ID: "k1", Key: fullKey, Enabled: true}},
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

// TestProviderTestFiltersDisabledByKeyIDs API Key 启用/停用对测试接口的过滤：
// key_ids 长度 1（单条测试）不过滤停用 Key，length>1 或省略时跳过停用 Key。
// 验证：批量（无 key_ids）跳过一个停用 Key；单条（key_ids=[disabled_id]）可发起测试。
func TestProviderTestFiltersDisabledByKeyIDs(t *testing.T) {
	s, _ := newTestServer(t)
	// mock 上游：每个请求响应 200
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer up.Close()

	pid := "p-key-enabled"
	if err := s.State.Mutate(func(ns *core.Snapshot) {
		ns.Providers = append(ns.Providers, &config.Provider{
			ID: pid, Name: "P", BaseURL: up.URL, APIType: "openai", Enabled: true,
			APIKeys: []config.ProviderAPIKey{
				{ID: "k1", Key: "sk-1", Enabled: true},
				{ID: "k2", Key: "sk-2", Enabled: false},
				{ID: "k3", Key: "sk-3", Enabled: true},
			},
			Models: []config.ProviderModel{{Name: "m"}},
		})
	}); err != nil {
		t.Fatal(err)
	}

	// 1) 批量测试（single=false）：应跳过 k2，仅对 k1/k3 发起测试
	atomic.StoreInt32(&hits, 0)
	body := fmt.Sprintf(`{"provider_id":%q,"base_url":%q,"api_type":"openai","api_keys":[{"id":"k1","key":"sk-1","enabled":true},{"id":"k2","key":"sk-2","enabled":false},{"id":"k3","key":"sk-3","enabled":true}],"headers":[],"model":"m","single":false}`,
		pid, up.URL)
	req := httptest.NewRequest("POST", "/api/providers/test", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleProviderTest(rec, req)
	if rec.Code != 200 {
		t.Fatalf("批量测试应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("批量测试应只发起 2 次（跳过停用 k2），实际 %d", got)
	}

	// 2) 单条测试 single=true key_ids=[k2]：不应跳过停用 Key（保留排查连通性能力）
	atomic.StoreInt32(&hits, 0)
	body = fmt.Sprintf(`{"provider_id":%q,"base_url":%q,"api_type":"openai","api_keys":[{"id":"k1","key":"sk-1","enabled":true},{"id":"k2","key":"sk-2","enabled":false},{"id":"k3","key":"sk-3","enabled":true}],"headers":[],"model":"m","key_ids":["k2"],"single":true}`,
		pid, up.URL)
	req = httptest.NewRequest("POST", "/api/providers/test", strings.NewReader(body))
	rec = httptest.NewRecorder()
	s.handleProviderTest(rec, req)
	if rec.Code != 200 {
		t.Fatalf("单条测试应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("单条测试应对停用 Key k2 发起 1 次（排查场景），实际 %d", got)
	}
}

// TestFetchModelsFormSkipsDisabled 自动获取模型跳过停用 Key，
// 表单未传 enabled 时从已保存配置回填（与编辑页回显一致）。
func TestFetchModelsFormSkipsDisabled(t *testing.T) {
	s, _ := newTestServer(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer up.Close()

	pid := "p-fetch-disabled"
	if err := s.State.Mutate(func(ns *core.Snapshot) {
		ns.Providers = append(ns.Providers, &config.Provider{
			ID: pid, Name: "P", BaseURL: up.URL, APIType: "openai", Enabled: true,
			APIKeys: []config.ProviderAPIKey{
				{ID: "k1", Key: "sk-1", Enabled: false}, // 已保存为停用
				{ID: "k2", Key: "sk-2", Enabled: true},
			},
		})
	}); err != nil {
		t.Fatal(err)
	}

	// 表单不传 enabled：后端应按已保存状态识别 k1 停用并跳过
	body := fmt.Sprintf(`{"provider_id":%q,"base_url":%q,"api_type":"openai","api_keys":[{"id":"k1","key":"sk-1"},{"id":"k2","key":"sk-2"}],"headers":[]}`,
		pid, up.URL)
	req := httptest.NewRequest("POST", "/api/providers/fetch-models", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleFetchModelsForm(rec, req)
	if rec.Code != 200 {
		t.Fatalf("应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "m1") {
		t.Errorf("应使用 k2 成功获取模型: %s", rec.Body.String())
	}
}

// TestApplyPayloadKeyEnabledMerge 表单 enabled 字段合并规则：
// 新增默认 true；修改时 nil 走 oldKeys 查回，*true/*false 显式覆盖。
// 脱敏回显"未修改"分支必须保留原 enabled 状态（用户改 Key 字符串时不动开关）。
func TestApplyPayloadKeyEnabledMerge(t *testing.T) {
	// 修改场景：原 Key k1=enabled=false, k2=enabled=true
	p := &config.Provider{
		ID: "p1", Name: "P", BaseURL: "https://x", Enabled: true, Weight: 1,
		APIKeys: []config.ProviderAPIKey{
			{ID: "k1", Key: "sk-old-1", Enabled: false},
			{ID: "k2", Key: "sk-old-2", Enabled: true},
		},
	}

	// 1) 全量提交 enabled：显式覆盖（p.APIKeys 保留作为 oldKeys 参考；applyPayload 内部会整体替换）
	p1 := *p
	body := &providerPayload{}
	body.Enabled = config.BoolPtr(true)
	body.APIKeys = append(body.APIKeys, struct {
		ID      string `json:"id"`
		Key     string `json:"key"`
		Enabled *bool  `json:"enabled"`
	}{ID: "k1", Key: "sk-new-1", Enabled: config.BoolPtr(true)})
	body.APIKeys = append(body.APIKeys, struct {
		ID      string `json:"id"`
		Key     string `json:"key"`
		Enabled *bool  `json:"enabled"`
	}{ID: "k2", Key: "sk-new-2", Enabled: config.BoolPtr(false)})
	applyPayload(&p1, body, false)
	if !p1.APIKeys[0].Enabled {
		t.Error("k1 显式 enabled=true 应覆盖原 false")
	}
	if p1.APIKeys[1].Enabled {
		t.Error("k2 显式 enabled=false 应覆盖原 true")
	}

	// 2) 脱敏回显"未修改"：保留原 enabled（即使表单 nil 也不被覆盖）
	masked1 := core.MaskKey("sk-old-1") // k1 原 false
	masked2 := core.MaskKey("sk-old-2") // k2 原 true
	p2 := *p
	body2 := &providerPayload{}
	body2.Enabled = config.BoolPtr(true)
	body2.APIKeys = append(body2.APIKeys, struct {
		ID      string `json:"id"`
		Key     string `json:"key"`
		Enabled *bool  `json:"enabled"`
	}{ID: "k1", Key: masked1, Enabled: nil})
	body2.APIKeys = append(body2.APIKeys, struct {
		ID      string `json:"id"`
		Key     string `json:"key"`
		Enabled *bool  `json:"enabled"`
	}{ID: "k2", Key: masked2, Enabled: nil})
	applyPayload(&p2, body2, false)
	if p2.APIKeys[0].Enabled {
		t.Error("k1 脱敏回显应保留原 false")
	}
	if !p2.APIKeys[1].Enabled {
		t.Error("k2 脱敏回显应保留原 true")
	}
}

// TestFetchModelsByIDSkipsDisabledKey GET /api/providers/fetch-models 按首条启用 Key 调用：
// 用户故意停用首条 Key（往往因已失效）时，系统应跳到下一条启用 Key，
// 全部停用时返回 400——与 2.3.2 启停用语义一致，避免固定 APIKeys[0] 撞上停用 Key。
func TestFetchModelsByIDSkipsDisabledKey(t *testing.T) {
	s, _ := newTestServer(t)
	var gotKey string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer up.Close()

	pid := "p-fetch-skip-disabled"
	if err := s.State.Mutate(func(ns *core.Snapshot) {
		ns.Providers = append(ns.Providers, &config.Provider{
			ID: pid, Name: "P", BaseURL: up.URL, APIType: "openai", Enabled: true,
			APIKeys: []config.ProviderAPIKey{
				{ID: "k1", Key: "sk-disabled", Enabled: false},
				{ID: "k2", Key: "sk-enabled", Enabled: true},
			},
		})
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/providers/fetch-models?provider_id="+pid, nil)
	rec := httptest.NewRecorder()
	s.handleFetchModelsByID(rec, req)
	if rec.Code != 200 {
		t.Fatalf("应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(gotKey, "sk-enabled") {
		t.Errorf("应使用启用 Key sk-enabled，实际 %q", gotKey)
	}
	if strings.Contains(gotKey, "sk-disabled") {
		t.Errorf("不应使用停用 Key sk-disabled，实际 %q", gotKey)
	}
}

// TestFetchModelsByIDAllDisabledReturns400 全部 Key 停用时返回 400 提示用户启用。
func TestFetchModelsByIDAllDisabledReturns400(t *testing.T) {
	s, _ := newTestServer(t)
	pid := "p-fetch-all-disabled"
	if err := s.State.Mutate(func(ns *core.Snapshot) {
		ns.Providers = append(ns.Providers, &config.Provider{
			ID: pid, Name: "P", BaseURL: "http://x", APIType: "openai", Enabled: true,
			APIKeys: []config.ProviderAPIKey{{ID: "k1", Key: "sk-x", Enabled: false}},
		})
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/providers/fetch-models?provider_id="+pid, nil)
	rec := httptest.NewRecorder()
	s.handleFetchModelsByID(rec, req)
	if rec.Code != 400 {
		t.Fatalf("全部 Key 停用应 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "全部") {
		t.Errorf("错误消息应提示「全部 Key 已停用」，实际 %s", rec.Body.String())
	}
}

// TestProviderTestSingleKeyProviderBatchSkipsDisabled 单 Key 供应商批量测试边界：
// 用 len(key_ids)==1 推断单条，1-Key 供应商批量模式会被误判为单条从而命中停用 Key。
// 改用显式 single 字段后，批量模式（single=false）应严格按 enabled 过滤。
func TestProviderTestSingleKeyProviderBatchSkipsDisabled(t *testing.T) {
	s, _ := newTestServer(t)
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer up.Close()

	pid := "p-single-disabled"
	if err := s.State.Mutate(func(ns *core.Snapshot) {
		ns.Providers = append(ns.Providers, &config.Provider{
			ID: pid, Name: "P", BaseURL: up.URL, APIType: "openai", Enabled: true,
			APIKeys: []config.ProviderAPIKey{{ID: "k1", Key: "sk-only", Enabled: false}},
			Models:  []config.ProviderModel{{Name: "m"}},
		})
	}); err != nil {
		t.Fatal(err)
	}

	// 批量：single=false，期望跳过停用 Key → 400
	body := fmt.Sprintf(`{"provider_id":%q,"base_url":%q,"api_type":"openai","api_keys":[{"id":"k1","key":"sk-only","enabled":false}],"headers":[],"model":"m","key_ids":["k1"],"single":false}`,
		pid, up.URL)
	req := httptest.NewRequest("POST", "/api/providers/test", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleProviderTest(rec, req)
	if rec.Code == 200 {
		t.Errorf("单 Key 供应商批量模式（single=false）应跳过停用 Key 不发起测试，实际 200 hits=%d", atomic.LoadInt32(&hits))
	}
	// 单条：single=true，期望可发起（保留对停用 Key 的排查能力）
	atomic.StoreInt32(&hits, 0)
	body = fmt.Sprintf(`{"provider_id":%q,"base_url":%q,"api_type":"openai","api_keys":[{"id":"k1","key":"sk-only","enabled":false}],"headers":[],"model":"m","key_ids":["k1"],"single":true}`,
		pid, up.URL)
	req = httptest.NewRequest("POST", "/api/providers/test", strings.NewReader(body))
	rec = httptest.NewRecorder()
	s.handleProviderTest(rec, req)
	if rec.Code != 200 || atomic.LoadInt32(&hits) != 1 {
		t.Errorf("单条模式（single=true）应可测试停用 Key，实际 code=%d hits=%d body=%s",
			rec.Code, atomic.LoadInt32(&hits), rec.Body.String())
	}
}

// TestDashboardProviderEnabledCountExcludesAllKeysDisabled 仪表盘 pEnabled 与路由层
// 同口径：供应商级 enabled=true 但全部 Key 停用时不计为「启用」，避免数字与实际能力不一致。
func TestDashboardProviderEnabledCountExcludesAllKeysDisabled(t *testing.T) {
	s, _ := newTestServer(t)
	if err := s.State.Mutate(func(ns *core.Snapshot) {
		ns.Providers = append(ns.Providers,
			&config.Provider{ID: "p-ok", Name: "OK", BaseURL: "http://x", APIType: "openai", Enabled: true,
				APIKeys: []config.ProviderAPIKey{{ID: "k", Key: "sk", Enabled: true}}},
			&config.Provider{ID: "p-dead", Name: "DEAD", BaseURL: "http://x", APIType: "openai", Enabled: true,
				APIKeys: []config.ProviderAPIKey{{ID: "k", Key: "sk", Enabled: false}}},
			&config.Provider{ID: "p-off", Name: "OFF", BaseURL: "http://x", APIType: "openai", Enabled: false,
				APIKeys: []config.ProviderAPIKey{{ID: "k", Key: "sk", Enabled: true}}},
		)
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/dashboard", nil)
	rec := httptest.NewRecorder()
	s.handleDashboard(rec, req)
	if rec.Code != 200 {
		t.Fatalf("应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	// 期望 enabled=1（仅 p-ok），dead 因全 Key 停用不计，off 供应商级未启用不计
	if !strings.Contains(rec.Body.String(), `"enabled":1`) {
		t.Errorf("仪表盘 pEnabled 应为 1（仅 p-ok），实际 body=%s", rec.Body.String())
	}
}
