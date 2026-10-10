package api

import (
	"strings"
	"testing"

	"llmproxy/internal/logsdb"
)

// TestMaskLogAPIKeys 日志列表 API Key 脱敏（前 3 位 + *** + 后 3 位，与 API Key 下拉框一致）。
func TestMaskLogAPIKeys(t *testing.T) {
	logs := []*logsdb.CallLog{
		{APIKey: "sk-proxy-e2e-key"},
		{APIKey: "short"},
		{APIKey: ""},
	}
	maskLogAPIKeys(logs)
	if got := logs[0].APIKey; got != "sk-***key" {
		t.Errorf("常规 Key 脱敏异常: %q", got)
	}
	if got := logs[1].APIKey; got != "***" {
		t.Errorf("短 Key 脱敏异常: %q", got)
	}
	if got := logs[2].APIKey; got != "" {
		t.Errorf("空 Key 脱敏异常: %q", got)
	}
}

// TestNormalizeTimeParamEndOfDay L7 回归：end_time 传纯日期时补全为当天 23:59:59
// （旧实现统一归一化为当天 00:00:00，直接调 API 传纯日期会漏掉当天全部日志）。
// v1.0.0（L1）：新增 ok 返回值——非法输入返回 false，调用方 400 拒绝
// （旧实现原样透传进 SQL 字符串比较：非法 start_time 恒假过滤全部、非法 end_time 恒真等于没过滤）。
func TestNormalizeTimeParamEndOfDay(t *testing.T) {
	// 起始时间：纯日期 → 当天 00:00:00
	if got, ok := normalizeTimeParam("2026-09-30", false); !ok || !strings.HasPrefix(got, "2026-09-30T00:00:00") {
		t.Errorf("start_time 纯日期应归一化为当天 00:00:00，实际 %q ok=%v", got, ok)
	}
	// 结束时间：纯日期 → 当天 23:59:59（v1.0.0 L7 修复点）
	if got, ok := normalizeTimeParam("2026-09-30", true); !ok || !strings.HasPrefix(got, "2026-09-30T23:59:59") {
		t.Errorf("end_time 纯日期应归一化为当天 23:59:59，实际 %q ok=%v", got, ok)
	}
	// 带时间的输入不受 isEnd 影响
	if got, ok := normalizeTimeParam("2026-09-30T12:00:00", true); !ok || !strings.HasPrefix(got, "2026-09-30T12:00:00") {
		t.Errorf("带时间的 end_time 不应被改动，实际 %q ok=%v", got, ok)
	}
	// 空 → 空串（合法：表示不限制）
	if got, ok := normalizeTimeParam("", true); !ok || got != "" {
		t.Errorf("空输入应返回空串，实际 %q ok=%v", got, ok)
	}
	// 非法输入 → ok=false（L1 修复点：不再原样透传进 SQL）
	if _, ok := normalizeTimeParam("abc", false); ok {
		t.Errorf("非法 start_time 'abc' 应返回 ok=false")
	}
	if _, ok := normalizeTimeParam("2026-13-99", true); ok {
		t.Errorf("非法日期 '2026-13-99' 应返回 ok=false")
	}
}
