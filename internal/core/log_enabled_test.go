package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"llmproxy/internal/config"
)

// TestLogEnabledSnapshotSync 日志记录开关在配置 → 快照 → 持久化全链路正确同步。
// 默认关闭；Mutate 开启后快照生效且 yml 显式落盘；外部改 yml 后 Reload 跟随新值。
func TestLogEnabledSnapshotSync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yml")
	mgr := config.NewManager(path)
	if _, err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	s := NewState(mgr)
	defer s.Stop()

	// 1) 默认关闭（config.Default 的 log.enabled=false）
	if s.Snap().LogEnabled {
		t.Fatal("默认配置下快照 LogEnabled 应为 false")
	}

	// 2) Mutate 开启：快照生效 + yml 落盘
	if err := s.MutateSettings(func(ns *Snapshot) { ns.LogEnabled = true }); err != nil {
		t.Fatal(err)
	}
	if !s.Snap().LogEnabled {
		t.Fatal("Mutate 开启后快照 LogEnabled 应为 true")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "enabled: true") {
		t.Errorf("开启后 yml 应显式落盘 enabled: true:\n%s", data)
	}

	// 3) 模拟外部手改 yml 为关闭：重新 Load（Watch 热加载的读文件动作）→ Reload 跟随新值
	y := strings.Replace(string(data), "enabled: true", "enabled: false", 1)
	if err := os.WriteFile(path, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Load(); err != nil { // 同 mgr.Watch：重新解析文件并替换内存配置
		t.Fatal(err)
	}
	s.Reload()
	if s.Snap().LogEnabled {
		t.Fatal("yml 改为 false 后 Reload，快照 LogEnabled 应为 false")
	}
}
