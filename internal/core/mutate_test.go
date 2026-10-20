package core

import (
	"path/filepath"
	"testing"

	"llmproxy/internal/config"
)

// TestMutatePreservesUnrelatedFields Mutate 修改某一类字段（APIKeys）时，
// 其余全部快照字段必须原样保留并同步落盘。
// 为防止 Snapshot 新增字段时手工逐字段复制遗漏（如历史上曾因遗漏
// DefaultUserAgent，导致设置默认 UA 后任何代理 Key / 供应商变更都把该字段
// 还原为空串并写回 yml，表现为设置项静默丢失），本测试遍历全部标量字段
// 作为防回归网。
func TestMutatePreservesUnrelatedFields(t *testing.T) {
	dir := t.TempDir()
	m := config.NewManager(filepath.Join(dir, "cfg.yml"))
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	s := NewState(m)
	defer s.Stop()

	// 先通过 MutateSettings 铺满全部设置类字段（模拟用户已完成的各项设置）
	want := Snapshot{
		Strategy:               "round_robin",
		CooldownEnabled:        false,
		CooldownSeconds:        45,
		CooldownRetryCount:     3,
		UpstreamTimeoutSeconds: 120,
		TestMessage:            "ping",
		DefaultUserAgent:       "llmproxy/1.0.0",
		IPTrustLevel:           2,
		LogDBPath:              "custom.db",
		LogEnabled:             true,
	}
	if err := s.MutateSettings(func(ns *Snapshot) {
		*ns = want
	}); err != nil {
		t.Fatal(err)
	}

	// 再走 Mutate 修改代理 Key（与设置项无关的另一类变更）
	nk := &config.ProxyKey{ID: NewID(), Name: "k1", Key: "sk-" + "a", Enabled: true}
	if err := s.Mutate(func(ns *Snapshot) {
		ns.APIKeys = append(ns.APIKeys, nk)
	}); err != nil {
		t.Fatal(err)
	}

	got := s.Snap()
	if got.DefaultUserAgent != want.DefaultUserAgent {
		t.Errorf("DefaultUserAgent 被 Mutate 还原: want %q got %q", want.DefaultUserAgent, got.DefaultUserAgent)
	}
	if got.Strategy != want.Strategy {
		t.Errorf("Strategy 被 Mutate 还原: want %q got %q", want.Strategy, got.Strategy)
	}
	if got.CooldownEnabled != want.CooldownEnabled || got.CooldownSeconds != want.CooldownSeconds || got.CooldownRetryCount != want.CooldownRetryCount {
		t.Errorf("Cooldown 被 Mutate 还原: got %+v", got)
	}
	if got.UpstreamTimeoutSeconds != want.UpstreamTimeoutSeconds {
		t.Errorf("UpstreamTimeoutSeconds 被 Mutate 还原: want %d got %d", want.UpstreamTimeoutSeconds, got.UpstreamTimeoutSeconds)
	}
	if got.TestMessage != want.TestMessage {
		t.Errorf("TestMessage 被 Mutate 还原: want %q got %q", want.TestMessage, got.TestMessage)
	}
	if got.IPTrustLevel != want.IPTrustLevel {
		t.Errorf("IPTrustLevel 被 Mutate 还原: want %d got %d", want.IPTrustLevel, got.IPTrustLevel)
	}
	if got.LogDBPath != want.LogDBPath || got.LogEnabled != want.LogEnabled {
		t.Errorf("Log 被 Mutate 还原: got path=%q enabled=%v", got.LogDBPath, got.LogEnabled)
	}
	if len(got.APIKeys) != 1 || got.APIKeys[0].Key != nk.Key {
		t.Errorf("本次变更的 APIKeys 未生效: %+v", got.APIKeys)
	}

	// yml 持久化同步校验：重新加载配置后字段仍保留（排除"仅内存对、文件已丢"）
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	cfg := m.Cfg()
	if cfg.DefaultUserAgent != want.DefaultUserAgent {
		t.Errorf("yml 中 DefaultUserAgent 丢失: want %q got %q", want.DefaultUserAgent, cfg.DefaultUserAgent)
	}
	if cfg.Routing.Strategy != want.Strategy || cfg.Cooldown.DurationSeconds != want.CooldownSeconds {
		t.Errorf("yml 中路由/冷却字段丢失: strategy=%q cooldown_seconds=%d", cfg.Routing.Strategy, cfg.Cooldown.DurationSeconds)
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0].Key != nk.Key {
		t.Errorf("yml 中 APIKeys 未落盘: %+v", cfg.APIKeys)
	}
}
