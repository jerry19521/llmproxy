package api

// v1.0.0 回归测试（对应需求 12 版本更新记录 v1.0.0 小节）：
//   全局默认 User-Agent（default_user_agent 设置项）——部分上游网关（腾讯元宝 bot）
//   对 Go 默认 UA 返回 403「错误请求」。覆盖：设置读写全链路、
//   测试 Key（/api/providers/test）与自动获取模型（/api/providers/fetch-models）
//   两处出站请求的 UA 优先级（供应商自定义头 > 全局默认 > Go 默认）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"llmproxy/internal/config"
	"llmproxy/internal/core"
)

// TestSettingsDefaultUserAgentRoundTrip 设置接口 default_user_agent：
// PUT 写入 → 快照生效 → GET 回读 → yml 持久化 → 清空回退。
func TestSettingsDefaultUserAgentRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := config.NewManager(filepath.Join(dir, "cfg.yml"))
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	state := core.NewState(m)
	defer state.Stop()
	s := &Server{Mgr: m, State: state, Listen: "127.0.0.1:0"}

	// PUT：设置全局默认 UA
	req := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(`{"default_user_agent":"llmproxy/1.8.6"}`))
	rec := httptest.NewRecorder()
	s.handleSettingsPut(rec, req)
	if rec.Code != 200 {
		t.Fatalf("PUT /api/settings status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := state.Snap().DefaultUserAgent; got != "llmproxy/1.8.6" {
		t.Errorf("快照未生效: %q", got)
	}

	// GET：回读
	req2 := httptest.NewRequest("GET", "/api/settings", nil)
	rec2 := httptest.NewRecorder()
	s.handleSettingsGet(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("GET /api/settings status=%d", rec2.Code)
	}
	var got struct {
		DefaultUserAgent string `json:"default_user_agent"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.DefaultUserAgent != "llmproxy/1.8.6" {
		t.Errorf("GET 回读异常: %q", got.DefaultUserAgent)
	}

	// yml 持久化
	data, err := os.ReadFile(filepath.Join(dir, "cfg.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "default_user_agent: llmproxy/1.8.6") {
		t.Errorf("yml 缺少 default_user_agent:\n%s", data)
	}

	// PUT：清空（回退 Go 默认）+ 纯空白 TrimSpace
	req3 := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(`{"default_user_agent":"   "}`))
	rec3 := httptest.NewRecorder()
	s.handleSettingsPut(rec3, req3)
	if rec3.Code != 200 {
		t.Fatalf("PUT 清空 status=%d", rec3.Code)
	}
	if got := state.Snap().DefaultUserAgent; got != "" {
		t.Errorf("纯空白应视为空（回退 Go 默认），实际: %q", got)
	}
}

// TestProviderTestUserAgentPriority 测试 Key 出站请求的 UA 优先级（经 mock 上游完整链路）。
// 未配置全局默认时 httptest 服务端会收到 transport 层补的 Go-http-client/1.1——
// 正是被腾讯元宝网关拦截的那个 UA，一并验证。
func TestProviderTestUserAgentPriority(t *testing.T) {
	var mu sync.Mutex
	var gotUA string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotUA = r.Header.Get("User-Agent")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()

	s, _ := newAuditServer(t)
	run := func(headers string) string {
		mu.Lock()
		gotUA = ""
		mu.Unlock()
		body := fmt.Sprintf(`{"base_url":%q,"api_type":"openai","api_keys":[{"key":"sk-x"}],"headers":%s,"model":"m"}`, up.URL, headers)
		req := httptest.NewRequest("POST", "/api/providers/test", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleProviderTest(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("mock 上游测试应 200，实际 %d：%s", rec.Code, rec.Body.String())
		}
		mu.Lock()
		defer mu.Unlock()
		return gotUA
	}

	// 未配置全局默认：transport 补 Go 默认（旧行为不变）
	if got := run("[]"); got != "Go-http-client/1.1" {
		t.Errorf("未配置全局默认时应为 Go 默认 UA，实际: %q", got)
	}
	// 配置全局默认：应用
	if err := s.State.MutateSettings(func(ns *core.Snapshot) { ns.DefaultUserAgent = "llmproxy/1.8.6" }); err != nil {
		t.Fatal(err)
	}
	if got := run("[]"); got != "llmproxy/1.8.6" {
		t.Errorf("无自定义头应用全局默认 UA，实际: %q", got)
	}
	// 供应商自定义 UA：覆盖全局默认
	if got := run(`[{"key":"User-Agent","value":"curl/8.10.1"}]`); got != "curl/8.10.1" {
		t.Errorf("供应商自定义 UA 应覆盖全局默认，实际: %q", got)
	}
}

// TestFetchModelsUserAgent 自动获取模型出站请求应用全局默认 UA（经 mock 上游完整链路）。
func TestFetchModelsUserAgent(t *testing.T) {
	var mu sync.Mutex
	var gotUA string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotUA = r.Header.Get("User-Agent")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m-a"},{"id":"m-b"}]}`))
	}))
	defer up.Close()

	s, _ := newAuditServer(t)
	if err := s.State.MutateSettings(func(ns *core.Snapshot) { ns.DefaultUserAgent = "llmproxy/1.8.6" }); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"base_url":%q,"api_type":"openai","api_keys":[{"key":"sk-x"}]}`, up.URL)
	req := httptest.NewRequest("POST", "/api/providers/fetch-models", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleFetchModelsForm(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mock 上游获取模型应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if gotUA != "llmproxy/1.8.6" {
		t.Errorf("fetch-models 应携带全局默认 UA，实际: %q", gotUA)
	}
}
