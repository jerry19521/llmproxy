package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestJWTExpireHoursNormalize expire_hours 为 0/负值时回退 24。
// 必须归一化：手写 yml 配 0 会导致登录成功但 token 即刻过期，
// 管理员被锁在门外且无任何提示。
func TestJWTExpireHoursNormalize(t *testing.T) {
	c := JWTConfig{ExpireHours: 0}
	c.normalize()
	if c.ExpireHours != 24 {
		t.Errorf("expire_hours=0 应回退 24, got %d", c.ExpireHours)
	}
	c = JWTConfig{ExpireHours: -8}
	c.normalize()
	if c.ExpireHours != 24 {
		t.Errorf("expire_hours=-8 应回退 24, got %d", c.ExpireHours)
	}
	c = JWTConfig{ExpireHours: 48}
	c.normalize()
	if c.ExpireHours != 48 {
		t.Errorf("合法值 48 不应被改动, got %d", c.ExpireHours)
	}

	// 全链路：手写 yml 含 expire_hours: 0 → Load 后归一化为 24
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	yml := "listen:\n  host: \"0.0.0.0\"\n  port: 9080\nadmin:\n  username: admin\n  password: x\njwt:\n  secret: s\n  expire_hours: 0\n"
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(path)
	cfg, err := m.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JWT.ExpireHours != 24 {
		t.Errorf("Load 后 expire_hours=0 应归一化为 24, got %d", cfg.JWT.ExpireHours)
	}
}

// TestManagerStopIdempotent Stop 二次调用不 panic。
// 重复关闭 stopCh 会导致 close-of-closed-channel panic，
// 必须 select-default 模式防护。
func TestManagerStopIdempotent(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(filepath.Join(dir, "config.yml"))
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	m.Stop()
	m.Stop() // 第二次调用不应 panic
}

// TestCooldownNormalize 冷却池配置缺省值兜底：DurationSeconds ≤ 0 时回退 300 秒，
// RetryCount ≤ 0 时回退 1。
func TestCooldownNormalize(t *testing.T) {
	// 全空配置：默认 300 秒 / 1 次
	c := CooldownConfig{}
	c.normalize()
	if c.DurationSeconds != 300 {
		t.Errorf("空配置默认 300 秒, got %d", c.DurationSeconds)
	}
	if c.RetryCount != 1 {
		t.Errorf("RetryCount 缺省应为 1, got %d", c.RetryCount)
	}
	// Enabled bool 零值即 false，由 yml 显式 true 开启；不强制兜底 true

	// 显式停用与非法值兜底
	c2 := CooldownConfig{Enabled: false, DurationSeconds: -5, RetryCount: -1}
	c2.normalize()
	if c2.Enabled {
		t.Error("显式 enabled=false 应保留停用状态")
	}
	if c2.DurationSeconds != 300 || c2.RetryCount != 1 {
		t.Errorf("非法值应兜底: seconds=%d retry=%d", c2.DurationSeconds, c2.RetryCount)
	}
}

// TestLogEnabledDefaultOff 日志记录开关默认关闭——
// Default() 关闭、yml 未写 enabled 字段视为关闭、显式 enabled: true 才开启。
func TestLogEnabledDefaultOff(t *testing.T) {
	// 1) Default() 默认关闭
	if Default().Log.Enabled {
		t.Error("Default() 日志记录应默认关闭")
	}

	// 2) yml 不写 enabled 字段 → 关闭
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	yml := "listen: {host: 127.0.0.1, port: 9080}\nadmin: {username: admin, password: x}\nlog:\n  db_path: data.db\n"
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(path)
	cfg, err := m.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Enabled {
		t.Error("yml 不写 enabled 字段应视为关闭")
	}
	if cfg.Log.DBPath != "data.db" {
		t.Errorf("db_path 解析异常: %q", cfg.Log.DBPath)
	}

	// 3) 显式 enabled: true → 开启
	if err := os.WriteFile(path, []byte(yml+"  enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m2 := NewManager(path)
	cfg2, err := m2.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg2.Log.Enabled {
		t.Error("显式 enabled: true 应开启日志记录")
	}
}

// TestUpdateSnapshotIsolation Update 必须基于最新快照原子修改：
// 1）并发 Update 不丢失更新——加锁串行化避免 read-modify-write write-skew；
// 2）已发布的旧快照不被原地修改（copy-on-write，Cfg() 的文档承诺）。
// 本机无 gcc 无法跑 -race，本测试从语义层面锁定两点。
func TestUpdateSnapshotIsolation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte("listen: {host: 127.0.0.1, port: 40000}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(path)
	if _, err := mgr.Load(); err != nil {
		t.Fatal(err)
	}

	// 1) 快照不可变：Update 后发布新对象，旧快照保持原值
	old := mgr.Cfg()
	if err := mgr.Update(func(c *Config) { c.Admin.Username = "changed" }); err != nil {
		t.Fatal(err)
	}
	if got := mgr.Cfg(); got == old {
		t.Fatal("Update 应发布新快照对象（copy-on-write），不应原地修改旧对象")
	}
	if old.Admin.Username != "admin" {
		t.Errorf("旧快照被原地修改: username=%q（应保持 admin）", old.Admin.Username)
	}
	if mgr.Cfg().Admin.Username != "changed" {
		t.Errorf("新快照未生效: %q", mgr.Cfg().Admin.Username)
	}

	// 2) 并发 Update 不丢失更新：8 goroutine × 5 次 port+1，最终恰为 40040
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if err := mgr.Update(func(c *Config) { c.Listen.Port++ }); err != nil {
					t.Errorf("并发 Update 失败: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if got := mgr.Cfg().Listen.Port; got != 40040 {
		t.Errorf("并发 Update 丢失更新: port=%d（应 40040）", got)
	}
}

// TestProviderKeyEnabledRoundTrip enabled 字段随 Save/Load 完整往返：
// 保存时写出 enabled: true/false，重新加载后值不变。
func TestProviderKeyEnabledRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte("listen: {host: 127.0.0.1, port: 9080}\nadmin: {username: admin, password: x}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(path)
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	// 写入含启用/停用 Key 的供应商
	if err := m.Update(func(c *Config) {
		c.Providers = []*Provider{{
			ID: "p1", Name: "X", BaseURL: "https://x", Enabled: true, Weight: 1,
			APIKeys: []ProviderAPIKey{
				{ID: "k1", Key: "sk-a", Enabled: true},
				{ID: "k2", Key: "sk-b", Enabled: false},
			},
		}}
	}); err != nil {
		t.Fatal(err)
	}
	// 重新加载：两条 Key 的 enabled 状态应完整保留
	cfg, err := m.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Providers[0].APIKeys[0].Enabled {
		t.Error("k1 enabled=true 丢失")
	}
	if cfg.Providers[0].APIKeys[1].Enabled {
		t.Error("k2 enabled=false 被破坏")
	}
	// 保存的 yml 文本应明确写出 enabled 字段（不依赖 omitempty）
	data, _ := os.ReadFile(path)
	s := string(data)
	if !strings.Contains(s, "enabled: true") || !strings.Contains(s, "enabled: false") {
		t.Errorf("保存的 yml 应同时包含 enabled: true / enabled: false:\n%s", s)
	}
}
