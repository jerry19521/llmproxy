package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestExtractClientIPLevels 验证按「IP 层级」提取来源 IP（需求 3.1 / 3.2.3）：
//
//	level=0：直连 IP（RemoteAddr）
//	level=1：XFF 首个 IP
//	level=2：XFF 倒数第二个 IP（两层代理下的真实客户端）
//	level=3：XFF 倒数第三个 IP，依此类推
//	XFF 不足时回退直连 IP。
func TestExtractClientIPLevels(t *testing.T) {
	mk := func(xff string, level int) string {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.0.0.9:1234" // 最接近本服务的代理 IP（RemoteAddr）
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return ExtractClientIP(r, level)
	}
	cases := []struct {
		name  string
		xff   string
		level int
		want  string
	}{
		{"level0 直连", "203.0.113.7, 10.0.0.1, 10.0.0.2", 0, "10.0.0.9"},
		{"level1 取首个", "203.0.113.7", 1, "203.0.113.7"},
		{"level2 取倒数第二", "203.0.113.7, 10.0.0.1", 2, "203.0.113.7"},
		{"level2 客户端伪造 XFF", "198.51.100.99, 203.0.113.7, 10.0.0.1", 2, "203.0.113.7"},
		{"level3 取倒数第三", "203.0.113.7, 10.0.0.1, 10.0.0.2", 3, "203.0.113.7"},
		{"level 超出 XFF 长度回退首个", "203.0.113.7", 3, "203.0.113.7"},
		{"无 XFF 回退直连", "", 2, "10.0.0.9"},
	}
	for _, c := range cases {
		if got := mk(c.xff, c.level); got != c.want {
			t.Errorf("%s: level=%d xff=%q got=%q want=%q", c.name, c.level, c.xff, got, c.want)
		}
	}
}

func TestMatchIPWhitelist(t *testing.T) {
	cases := []struct {
		ip    string
		rules []string
		want  bool
	}{
		{"1.2.3.4", []string{"*"}, true},
		{"192.168.1.5", []string{"192.168.*"}, true},
		{"192.169.1.5", []string{"192.168.*"}, false},
		{"10.0.0.1", []string{"192.168.*", "10.0.*"}, true},
		{"10.0.0.1", []string{"10.0.0.1"}, true},
		{"10.0.0.2", []string{"10.0.0.1"}, false},
		{"10.0.0.1", nil, false},
		{"10.0.0.1", []string{""}, false},
		// 八位组边界与归一化
		{"10.11.2.3", []string{"10.1.*"}, false},          // 短前缀不越权（尾点充当边界）
		{"10.1.7.9", []string{"10.1.*"}, true},            // 正常前缀命中
		{"10.1.7.9", []string{"10.*"}, true},              // 单段前缀
		{"101.1.1.1", []string{"10*"}, false},             // 非规范缺尾点规则忽略（fail-safe）
		{"10.11.2.3", []string{"10.1*"}, false},           // 非规范缺尾点规则忽略
		{"FE80::1", []string{"fe80::1"}, true},            // IPv6 大小写归一精确匹配
		{"fe80:0:0:0:0:0:0:1", []string{"fe80::1"}, true}, // IPv6 压缩形式归一
		{"fe80::2", []string{"fe80::1"}, false},           // 不同 IPv6 不匹配
	}
	for _, c := range cases {
		if got := MatchIPWhitelist(c.ip, c.rules); got != c.want {
			t.Errorf("MatchIPWhitelist(%q, %v) = %v, want %v", c.ip, c.rules, got, c.want)
		}
	}
}
