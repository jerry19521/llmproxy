// proxy_test.go：输出速率与生成耗时计算单元测试。
package proxy

import (
	"net/http"
	"strings"
	"testing"
)

func TestOutputTokensPerSecond(t *testing.T) {
	cases := []struct {
		name       string
		compl, gen int64
		want       float64
	}{
		{"token为0", 0, 1000, 0},
		{"耗时为0", 300, 0, 0},
		{"负值防护", -1, 100, 0},
		{"整秒", 300, 1000, 300},
		{"两秒", 100, 2000, 50},
		{"毫秒级", 10, 200, 50},
		{"小数速率", 150, 3000, 50},
		{"高频场景", 100000, 5000, 20000},
	}
	for _, c := range cases {
		if got := outputTokensPerSecond(c.compl, c.gen); got != c.want {
			t.Errorf("%s: outputTokensPerSecond(%d, %d) = %v, want %v", c.name, c.compl, c.gen, got, c.want)
		}
	}
}

func TestGenerationMs(t *testing.T) {
	// 口径（v1.0.0）：流式 = 末字节 - 上游首字节（模型实际吐字窗口）；
	// 非流式 = 整个请求耗时；流式异常（无首字节/末字节早于首字节）回退总耗时。
	cases := []struct {
		name                           string
		duration, firstByte, lastChunk int64
		streamed                       bool
		want                           int64
	}{
		{"流式：末字节-首字节", 500, 100, 480, true, 380},
		{"流式：首字节即结束", 500, 100, 100, true, 500},
		{"流式：末字节早于首字节（防护）", 500, 600, 300, true, 500},
		{"流式：未收到数据", 500, -1, 500, true, 500},
		{"非流式：取总耗时", 500, 100, 480, false, 500},
		{"非流式：未收到数据", 0, -1, 0, false, 0},
	}
	for _, c := range cases {
		r := &upstreamResult{DurationMs: c.duration, FirstByteMs: c.firstByte, LastChunkMs: c.lastChunk}
		if got := generationMs(r, c.streamed); got != c.want {
			t.Errorf("%s: generationMs(%d, %d, %d, %v) = %d, want %d", c.name, c.duration, c.firstByte, c.lastChunk, c.streamed, got, c.want)
		}
	}
	if got := generationMs(nil, true); got != 0 {
		t.Errorf("generationMs(nil) = %d, want 0", got)
	}
}

// TestMaskAuthHeaders 上游请求头认证字段脱敏（v1.0.0 日志结构）。
func TestMaskAuthHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer sk-0123456789abcdefXYZW")
	h.Set("X-Api-Key", "sk-ant-api03-0123456789abcdef")
	h.Set("User-Agent", "llmproxy/test")

	out := maskAuthHeaders(h)
	if out["Authorization"] != "Bearer sk-...XYZW" {
		t.Errorf("Authorization 脱敏异常: %s", out["Authorization"])
	}
	if out["X-Api-Key"] != "sk-ant-api...cdef" {
		t.Errorf("X-Api-Key 脱敏异常: %s", out["X-Api-Key"])
	}
	if out["Content-Type"] != "application/json" || out["User-Agent"] != "llmproxy/test" {
		t.Errorf("非认证头不应脱敏: %+v", out)
	}
	// 明文密钥不得出现
	for k, v := range out {
		if k != "User-Agent" && strings.Contains(v, "0123456789abcdef") {
			t.Errorf("头 %s 泄露明文密钥: %s", k, v)
		}
	}
	// 短值全遮蔽
	if got := maskSecret("short-key"); got != "*********" {
		t.Errorf("短值应全遮蔽: %s", got)
	}
}
