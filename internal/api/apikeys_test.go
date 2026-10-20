package api

import (
	"strings"
	"testing"
	"time"
)

// TestNormalizeExpiresAt 有效期归一化多布局兼容（BUG-1 回归测试）。
// 前端 datetime/date 控件产生的无时区时间，后端必须能解析并补全时区。
func TestNormalizeExpiresAt(t *testing.T) {
	// RFC3339 带时区 → 原样保留
	got, err := normalizeExpiresAt("2027-01-01T23:59:59+08:00")
	if err != nil || got != "2027-01-01T23:59:59+08:00" {
		t.Errorf("RFC3339 = %q, err = %v", got, err)
	}
	got, err = normalizeExpiresAt("2027-01-01T15:59:59Z")
	if err != nil || got != "2027-01-01T15:59:59Z" {
		t.Errorf("RFC3339 UTC = %q, err = %v", got, err)
	}

	// BUG-1 触发格式：无时区日期时间（前端曾直接拼接 'YYYY-MM-DDT23:59:59'）
	got, err = normalizeExpiresAt("2027-01-01T23:59:59")
	if err != nil {
		t.Fatalf("无时区日期时间应解析成功: %v", err)
	}
	if ts, perr := time.Parse(time.RFC3339, got); perr != nil || ts.Year() != 2027 {
		t.Errorf("无时区日期时间归一化结果非法: %q (%v)", got, perr)
	}

	// 无秒无时区（datetime-local 输出）→ 补 :00 秒
	got, err = normalizeExpiresAt("2027-01-01T23:59")
	if err != nil {
		t.Fatalf("无秒无时区应解析成功: %v", err)
	}
	if !strings.HasPrefix(got, "2027-01-01T23:59:00") {
		t.Errorf("无秒格式归一化异常: %q", got)
	}

	// 纯日期 → 当天 23:59:59（本地时区）
	got, err = normalizeExpiresAt("2027-01-01")
	if err != nil {
		t.Fatalf("纯日期应解析成功: %v", err)
	}
	if !strings.HasPrefix(got, "2027-01-01T23:59:59") {
		t.Errorf("纯日期归一化异常: %q", got)
	}

	// 空白 → 空串（永久有效）
	if got, err = normalizeExpiresAt("   "); err != nil || got != "" {
		t.Errorf("空白输入应返回空串: %q, %v", got, err)
	}

	// 非法格式 → 报错
	for _, bad := range []string{"2027/01/01", "not-a-date", "2027-13-01"} {
		if _, err := normalizeExpiresAt(bad); err == nil {
			t.Errorf("非法格式 %q 应报错", bad)
		}
	}
}

// TestValidProxyKeyFormat L2 代理 Key 格式校验（需求 2.7.2：sk- + 32 位字母数字）。
func TestValidProxyKeyFormat(t *testing.T) {
	valid := []string{
		"sk-abcdefghijklmnopqrstuvwxyz012345", // 26 字母 + 6 数字 = 32
		"sk-ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef", // 大写 26 + 小写 6 = 32
		"sk-0123456789abcdefghijklmnopqrstuv", // 数字 10 + 字母 22 = 32
	}
	for _, k := range valid {
		if !validProxyKeyFormat(k) {
			t.Errorf("合法格式应通过: %q", k)
		}
	}
	invalid := []string{
		"",                                     // 空
		"abc",                                  // 无 sk- 前缀
		"sk-",                                  // 无随机部分
		"sk-short1234",                         // 长度不足 32
		"sk-abcdefghijklmnopqrstuvwxyz0123456", // 长度超过 32
		"sk-abcdef-ghij+klmn!opqrstuvwxy0123",  // 含非法字符（- + !）
		"SK-abcdefghijklmnopqrstuvwxyz012345",  // 前缀大小写敏感
	}
	for _, k := range invalid {
		if validProxyKeyFormat(k) {
			t.Errorf("非法格式应被拒绝: %q", k)
		}
	}
}
