package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"llmproxy/internal/config"
	"llmproxy/internal/convert"
	"llmproxy/internal/core"
	"llmproxy/internal/logsdb"
)

// TestUpstream400NoRetry 验证上游返回 400 时不重试并归一化 502（需求 6.4 / 6.6），
// 流式与非流式路径行为一致。
// 回归背景：流式路径曾因 shouldRetry 先判 errStr 后判状态码，导致 400 被当作
// 可重试错误——换满全部 Key 重试并最终返回 503，违反「400 不重试、返回 502」。
func TestUpstream400NoRetry(t *testing.T) {
	var calls int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"mock bad request","type":"invalid_request_error"}}`))
	}))
	defer up.Close()

	dir := t.TempDir()
	store, err := logsdb.Open(filepath.Join(dir, "test-log.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mgr := config.NewManager(filepath.Join(dir, "cfg.yml"))
	if _, err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	state := core.NewState(mgr)
	defer state.Stop()
	err = state.Mutate(func(ns *core.Snapshot) {
		ns.Providers = []*config.Provider{{
			ID: "p1", Name: "p-400", BaseURL: up.URL + "/v1", APIType: convert.FormatOpenAI,
			Enabled: true, Weight: 1,
			// 3 条 Key：若误重试，上游请求次数会大于 1
			APIKeys: []config.ProviderAPIKey{
				{ID: "k1", Key: "sk-a"}, {ID: "k2", Key: "sk-b"}, {ID: "k3", Key: "sk-c"},
			},
			Models: []config.ProviderModel{{Name: "m-openai"}},
		}}
		ns.APIKeys = []*config.ProxyKey{{ID: "pk1", Name: "e2e", Key: "sk-proxy-e2e-key", Enabled: true, IPWhitelist: []string{"*"}}}
		ns.LogEnabled = true // 测试栈显式开启日志（v1.8.1 起默认关闭）
	})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(state)
	h.SetLogStore(store)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"非流式", `{"model":"@MODEL@","messages":[{"role":"user","content":"hi"}]}`},
		{"流式", `{"model":"@MODEL@","messages":[{"role":"user","content":"hi"}],"stream":true}`},
	} {
		atomic.StoreInt64(&calls, 0)
		status, body, _ := postProxy(t, h, convert.FormatOpenAI, "m-openai", tc.body, nil)
		n := atomic.LoadInt64(&calls)
		if status != http.StatusBadGateway {
			t.Errorf("[%s] 上游 400 应归一化为 502，实际 %d（body=%s）", tc.name, status, body)
		}
		if n != 1 {
			t.Errorf("[%s] 上游 400 不应重试，期望请求 1 次，实际 %d 次", tc.name, n)
		}
	}
}

// TestUpstream500RetriesAllKeys 验证可重试错误（5xx）仍按需求 6.4 换 Key 重试。
func TestUpstream500RetriesAllKeys(t *testing.T) {
	var calls int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"mock internal error"}}`))
	}))
	defer up.Close()

	dir := t.TempDir()
	store, err := logsdb.Open(filepath.Join(dir, "test-log.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mgr := config.NewManager(filepath.Join(dir, "cfg.yml"))
	if _, err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	state := core.NewState(mgr)
	defer state.Stop()
	err = state.Mutate(func(ns *core.Snapshot) {
		ns.Providers = []*config.Provider{{
			ID: "p1", Name: "p-500", BaseURL: up.URL + "/v1", APIType: convert.FormatOpenAI,
			Enabled: true, Weight: 1,
			APIKeys: []config.ProviderAPIKey{
				{ID: "k1", Key: "sk-a"}, {ID: "k2", Key: "sk-b"}, {ID: "k3", Key: "sk-c"},
			},
			Models: []config.ProviderModel{{Name: "m-openai"}},
		}}
		ns.APIKeys = []*config.ProxyKey{{ID: "pk1", Name: "e2e", Key: "sk-proxy-e2e-key", Enabled: true, IPWhitelist: []string{"*"}}}
		// 本用例专注重试语义：禁用冷却池，避免非流式子用例的失败入池后拦截流式子用例
		ns.CooldownEnabled = false
		ns.LogEnabled = true // 测试栈显式开启日志（v1.8.1 起默认关闭）
	})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(state)
	h.SetLogStore(store)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"非流式", `{"model":"@MODEL@","messages":[{"role":"user","content":"hi"}]}`},
		{"流式", `{"model":"@MODEL@","messages":[{"role":"user","content":"hi"}],"stream":true}`},
	} {
		atomic.StoreInt64(&calls, 0)
		status, _, _ := postProxy(t, h, convert.FormatOpenAI, "m-openai", tc.body, nil)
		n := atomic.LoadInt64(&calls)
		if status != http.StatusServiceUnavailable {
			t.Errorf("[%s] 全部 Key 失败应返回 503，实际 %d", tc.name, status)
		}
		if n != 3 {
			t.Errorf("[%s] 5xx 应换满 3 条 Key 重试，期望 3 次上游请求，实际 %d 次", tc.name, n)
		}
	}
}
