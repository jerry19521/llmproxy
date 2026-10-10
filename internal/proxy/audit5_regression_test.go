package proxy

import (
	"context"
	"testing"

	"llmproxy/internal/config"
)

// v1.8.6 回归测试（对应需求 12 版本更新记录 v1.8.6 小节）：
// 上游请求 User-Agent 三层优先级——供应商自定义头 > 全局默认（default_user_agent）> Go 默认。
// 背景：腾讯元宝 bot 网关对 Go 默认 UA（Go-http-client/1.1）返回 403「错误请求」，
// 复现记录见需求文档 v1.8.6 小节（同请求体下 curl UA 200 / Go 默认 UA 403 / 自标识 UA 200）。

// TestNewUpstreamRequestUserAgentPriority 真实转发路径（newUpstreamRequest）的 UA 优先级。
func TestNewUpstreamRequestUserAgentPriority(t *testing.T) {
	cases := []struct {
		name      string
		defaultUA string
		headers   []config.ProviderHeader
		wantSet   bool   // 期望是否显式设置 UA 头
		wantUA    string // 期望值（wantSet=false 时忽略）
	}{
		{"未配置全局+无自定义头：不设置", "", nil, false, ""},
		{"全局默认+无自定义头：用全局", "llmproxy/1.8.6", nil, true, "llmproxy/1.8.6"},
		{"全局默认+自定义UA：自定义优先", "llmproxy/1.8.6", []config.ProviderHeader{{Key: "User-Agent", Value: "curl/8.10.1"}}, true, "curl/8.10.1"},
		{"未配置全局+自定义UA：用自定义", "", []config.ProviderHeader{{Key: "User-Agent", Value: "myapp/1.0"}}, true, "myapp/1.0"},
		{"自定义头其他键不影响UA", "llmproxy/1.8.6", []config.ProviderHeader{{Key: "X-Forwarded-For", Value: "119.45.46.124"}}, true, "llmproxy/1.8.6"},
	}
	for _, c := range cases {
		p := &config.Provider{Headers: c.headers}
		req, err := newUpstreamRequest(context.Background(), p, "sk-test",
			"http://up.example/v1/chat/completions", []byte("{}"), "openai", c.defaultUA)
		if err != nil {
			t.Fatalf("%s: 构建请求失败: %v", c.name, err)
		}
		if !c.wantSet {
			if _, ok := req.Header["User-Agent"]; ok {
				t.Errorf("%s: 不应显式设置 User-Agent（交由 transport 补 Go 默认），实际: %q", c.name, req.Header.Get("User-Agent"))
			}
			continue
		}
		if got := req.Header.Get("User-Agent"); got != c.wantUA {
			t.Errorf("%s: UA=%q, want %q", c.name, got, c.wantUA)
		}
	}
}
