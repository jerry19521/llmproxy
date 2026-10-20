package convert

import (
	"net/http"
	"testing"
)

// 回归测试：
// 全局默认 User-Agent（default_user_agent）——部分上游网关（腾讯元宝 bot）
// 对 Go 默认 UA 返回 403「错误请求」，X-Forwarded-For 正确也无效。

// TestSetDefaultUserAgent 设置语义：空 UA 不设置头（保持 Go 标准库默认）、
// 非空 UA 设置、供应商自定义头（后应用）可覆盖全局默认。
func TestSetDefaultUserAgent(t *testing.T) {
	// 空：不设置任何头（transport 层补 Go-http-client/1.1，与旧行为一致）
	h := http.Header{}
	SetDefaultUserAgent(h, "")
	if _, ok := h["User-Agent"]; ok {
		t.Errorf("空 UA 不应设置 User-Agent 头，实际: %q", h.Get("User-Agent"))
	}
	// 非空：设置
	h2 := http.Header{}
	SetDefaultUserAgent(h2, "llmproxy/1.0.0")
	if got := h2.Get("User-Agent"); got != "llmproxy/1.0.0" {
		t.Errorf("UA 应为 llmproxy/1.0.0，实际: %q", got)
	}
	// 供应商自定义头（后应用）覆盖全局默认
	h3 := http.Header{}
	SetDefaultUserAgent(h3, "llmproxy/1.0.0")
	h3.Set("User-Agent", "curl/8.10.1")
	if got := h3.Get("User-Agent"); got != "curl/8.10.1" {
		t.Errorf("供应商自定义 UA 应覆盖全局默认，实际: %q", got)
	}
}
