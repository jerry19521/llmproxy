package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestJWTExpireHoursNormalize L2 回归：expire_hours 为 0/负值时回退 24
// （旧实现无校验，手写 yml 配 0 会导致登录成功但 token 即刻过期，管理员被锁在门外无提示）。
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

// TestManagerStopIdempotent L4 回归：Stop 二次调用不 panic
// （旧实现裸 close(stopCh)，与 core.State.Stop 的防护风格不一致，属地雷）。
func TestManagerStopIdempotent(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(filepath.Join(dir, "config.yml"))
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	m.Stop()
	m.Stop() // 第二次调用不应 panic
}

// TestCooldownNormalize 冷却池配置归一化：旧字段迁移、缺省值兜底。
func TestCooldownNormalize(t *testing.T) {
	// 旧版配置：duration_minutes 迁移为秒，enabled 缺省视为启用
	c := CooldownConfig{DurationMinutes: 2}
	c.normalize()
	if c.DurationSeconds != 120 {
		t.Errorf("duration_minutes=2 应迁移为 duration_seconds=120, got %d", c.DurationSeconds)
	}
	if !c.EnabledVal() {
		t.Error("旧配置无 enabled 字段应默认启用")
	}
	if c.RetryCount != 1 {
		t.Errorf("RetryCount 缺省应为 1, got %d", c.RetryCount)
	}
	if c.DurationMinutes != 0 {
		t.Errorf("归一化后旧字段应清零, got %d", c.DurationMinutes)
	}

	// 全空配置：默认 300 秒
	c2 := CooldownConfig{}
	c2.normalize()
	if c2.DurationSeconds != 300 {
		t.Errorf("空配置默认 300 秒, got %d", c2.DurationSeconds)
	}

	// 新配置：显式停用与非法值兜底
	f := false
	c3 := CooldownConfig{Enabled: &f, DurationSeconds: -5, RetryCount: -1}
	c3.normalize()
	if c3.EnabledVal() {
		t.Error("显式 enabled=false 应保留停用状态")
	}
	if c3.DurationSeconds != 300 || c3.RetryCount != 1 {
		t.Errorf("非法值应兜底: seconds=%d retry=%d", c3.DurationSeconds, c3.RetryCount)
	}
}

// TestLoadLegacyCooldownConfig 加载旧版 config.yml（duration_minutes）自动迁移为秒，
// 且保存后不再输出旧字段。
func TestLoadLegacyCooldownConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	legacy := `listen:
  host: "0.0.0.0"
  port: 9080
admin:
  username: "admin"
  password: "x"
cooldown:
  duration_minutes: 2
`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(path)
	cfg, err := m.Load()
	if err != nil {
		t.Fatalf("加载旧配置失败: %v", err)
	}
	if cfg.Cooldown.DurationSeconds != 120 {
		t.Errorf("duration_minutes=2 应迁移为 120 秒, got %d", cfg.Cooldown.DurationSeconds)
	}
	if !cfg.Cooldown.EnabledVal() {
		t.Error("旧配置应默认启用冷却池")
	}

	// 保存后 yml 不再含 duration_minutes，且含新字段
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if strings.Contains(s, "duration_minutes") {
		t.Errorf("保存后不应再输出旧字段 duration_minutes:\n%s", s)
	}
	if !strings.Contains(s, "duration_seconds: 120") || !strings.Contains(s, "retry_count: 1") || !strings.Contains(s, "enabled: true") {
		t.Errorf("保存后应输出新字段:\n%s", s)
	}

	// 重新加载：迁移结果稳定
	cfg2, err := m.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Cooldown.DurationSeconds != 120 {
		t.Errorf("重载后应为 120 秒, got %d", cfg2.Cooldown.DurationSeconds)
	}
}

// TestLogEnabledDefaultOff v1.0.0 回归：日志记录开关默认关闭——
// Default() 关闭、旧配置无 log.enabled 字段视为关闭（升级后停记）、显式 enabled: true 才开启。
func TestLogEnabledDefaultOff(t *testing.T) {
	// 1) Default() 默认关闭
	if Default().Log.EnabledVal() {
		t.Error("Default() 日志记录应默认关闭")
	}

	// 2) 旧配置无 enabled 字段 → 关闭（从旧版本升级后日志自动停记）
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	legacy := "listen: {host: 127.0.0.1, port: 9080}\nadmin: {username: admin, password: x}\nlog:\n  db_path: legacy.db\n"
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(path)
	cfg, err := m.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.EnabledVal() {
		t.Error("旧配置无 log.enabled 字段应视为关闭")
	}
	if cfg.Log.DBPath != "legacy.db" {
		t.Errorf("db_path 解析异常: %q", cfg.Log.DBPath)
	}

	// 3) 显式 enabled: true → 开启
	if err := os.WriteFile(path, []byte(legacy+"  enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m2 := NewManager(path)
	cfg2, err := m2.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg2.Log.EnabledVal() {
		t.Error("显式 enabled: true 应开启日志记录")
	}
}

// TestUpdateSnapshotIsolation H4 回归：Update 必须基于最新快照原子修改——
// 并发 Update 不丢失更新（旧实现无锁 read-modify-write 存在 write-skew 互相覆盖）；
// 且已发布的旧快照不被原地修改（copy-on-write，Cfg() 的文档承诺）。
// 本机无 gcc 无法跑 -race，本测试从语义层面锁定 H4 的两处修复点。
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
		t.Errorf("并发 Update 丢失更新: port=%d（应 40040）——H4 write-skew 复现", got)
	}
}
