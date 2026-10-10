package api

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"llmproxy/internal/config"
	"llmproxy/internal/core"
)

// TestSettingsCooldownRoundTrip 设置接口冷却池新字段（enabled/duration_seconds/retry_count）
// 写入 → 回读 → yml 持久化全链路。
func TestSettingsCooldownRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := config.NewManager(filepath.Join(dir, "cfg.yml"))
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	state := core.NewState(m)
	defer state.Stop()
	s := &Server{Mgr: m, State: state, Listen: "127.0.0.1:0"}

	// PUT：停用 + 45 秒 + 重试 3 次
	putBody := `{"cooldown":{"enabled":false,"duration_seconds":45,"retry_count":3}}`
	req := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(putBody))
	rec := httptest.NewRecorder()
	s.handleSettingsPut(rec, req)
	if rec.Code != 200 {
		t.Fatalf("PUT /api/settings status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 快照已生效
	snap := state.Snap()
	if snap.CooldownEnabled || snap.CooldownSeconds != 45 || snap.CooldownRetryCount != 3 {
		t.Errorf("快照未生效: enabled=%v seconds=%d retry=%d", snap.CooldownEnabled, snap.CooldownSeconds, snap.CooldownRetryCount)
	}

	// GET：回读
	req2 := httptest.NewRequest("GET", "/api/settings", nil)
	rec2 := httptest.NewRecorder()
	s.handleSettingsGet(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("GET /api/settings status=%d", rec2.Code)
	}
	var got struct {
		Cooldown struct {
			Enabled         bool `json:"enabled"`
			DurationSeconds int  `json:"duration_seconds"`
			RetryCount      int  `json:"retry_count"`
		} `json:"cooldown"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Cooldown.Enabled || got.Cooldown.DurationSeconds != 45 || got.Cooldown.RetryCount != 3 {
		t.Errorf("GET 回读异常: %+v", got.Cooldown)
	}

	// yml 持久化：含新字段、不含旧字段
	data, err := os.ReadFile(filepath.Join(dir, "cfg.yml"))
	if err != nil {
		t.Fatal(err)
	}
	y := string(data)
	if strings.Contains(y, "duration_minutes") {
		t.Errorf("yml 不应含旧字段 duration_minutes:\n%s", y)
	}
	for _, want := range []string{"enabled: false", "duration_seconds: 45", "retry_count: 3"} {
		if !strings.Contains(y, want) {
			t.Errorf("yml 缺少 %q:\n%s", want, y)
		}
	}

	// 非法值（0/负数）不覆盖现有配置
	putBody2 := `{"cooldown":{"duration_seconds":0,"retry_count":0}}`
	req3 := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(putBody2))
	rec3 := httptest.NewRecorder()
	s.handleSettingsPut(rec3, req3)
	if rec3.Code != 200 {
		t.Fatalf("PUT 非法值 status=%d", rec3.Code)
	}
	snap2 := state.Snap()
	if snap2.CooldownSeconds != 45 || snap2.CooldownRetryCount != 3 || snap2.CooldownEnabled {
		t.Errorf("非法值不应覆盖现有配置: %+v", snap2)
	}
}
