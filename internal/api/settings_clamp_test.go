package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSettingsIPTrustLevelUpperBound ip_trust_level 保存时只归零负值、无上限
// 约束：超大值会原样写入配置（XFF 层级实际不可能超过个位数），值域应 clamp
// 到上限，防止异常输入进入配置与日志。
func TestSettingsIPTrustLevelUpperBound(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(`{"ip_trust_level": 9999}`))
	rec := httptest.NewRecorder()
	s.handleSettingsPut(rec, req)
	if rec.Code != 200 {
		t.Fatalf("PUT /api/settings status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := s.State.Snap().IPTrustLevel; got != 16 {
		t.Fatalf("超大 ip_trust_level 应 clamp 到 16，实际 %d", got)
	}
}
