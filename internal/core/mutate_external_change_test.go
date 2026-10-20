package core

// 外部编辑配置与界面保存的 write-skew 回归测试（core 侧）：
// 保存前吸收外部变更，避免界面保存静默覆盖外部修改。

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"llmproxy/internal/config"
)

// TestMutateAbsorbsExternalChange 外部编辑 config.yml 落盘后、
// Watch 下一 tick 之前的窗口内执行界面保存（Mutate），外部变更必须被吸收保留——
// 否则基于内存旧快照全量写盘会把外部变更静默覆盖（且 lastMod 随之对齐
// 让 Watch 永远检测不到），变更完全丢失且无告警。
func TestMutateAbsorbsExternalChange(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cfg.yml")
	mgr := config.NewManager(cfgPath)
	if _, err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	state := NewState(mgr)
	defer state.Stop()

	// 初始：界面保存一个供应商
	if err := state.Mutate(func(ns *Snapshot) {
		ns.Providers = []*config.Provider{{ID: "p1", Name: "P1", BaseURL: "http://a", Enabled: true}}
	}); err != nil {
		t.Fatal(err)
	}

	// 模拟外部编辑：基于当前文件内容追加一个供应商后直接写盘
	c := mgr.Cfg()
	ext := *c
	ext.Providers = append(append([]*config.Provider{}, c.Providers...),
		&config.Provider{ID: "p-ext", Name: "External", BaseURL: "http://b", Enabled: true})
	data, err := yaml.Marshal(&ext)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // 确保 mtime 前进（触发变更检测）
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	// Watch tick 之前的窗口内执行界面保存
	if err := state.Mutate(func(ns *Snapshot) { ns.TestMessage = "ui-change" }); err != nil {
		t.Fatal(err)
	}

	got := mgr.Cfg()
	if got.TestMessage != "ui-change" {
		t.Fatalf("界面变更应生效，实际 TestMessage=%q", got.TestMessage)
	}
	found := false
	for _, p := range got.Providers {
		if p.ID == "p-ext" {
			found = true
		}
	}
	if !found {
		t.Fatalf("外部编辑的供应商被界面保存静默覆盖丢失，现存 %d 个供应商", len(got.Providers))
	}
}

// TestMutateRejectsBrokenExternalFile 外部把文件改坏（YAML 非法）时，
// 保存应被拒绝并返回错误——覆盖坏文件会让用户已输入的修复一并丢失。
func TestMutateRejectsBrokenExternalFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cfg.yml")
	mgr := config.NewManager(cfgPath)
	if _, err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	state := NewState(mgr)
	defer state.Stop()

	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(cfgPath, []byte("listen: [broken: yaml"), 0600); err != nil {
		t.Fatal(err)
	}

	err := state.Mutate(func(ns *Snapshot) { ns.TestMessage = "should-not-save" })
	if err == nil {
		t.Fatalf("外部文件被改坏时保存应被拒绝，实际保存成功")
	}
	if got := mgr.Cfg().TestMessage; got == "should-not-save" {
		t.Fatalf("拒绝保存时内存配置不应被变更")
	}
}
