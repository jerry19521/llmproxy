package core

import (
	"os"
	"path/filepath"
	"testing"

	"llmproxy/internal/config"
)

// mkStateFromYML 从临时配置构建 State（测试辅助）。
func mkStateFromYML(t *testing.T, yml string) *State {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cfg.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	m := config.NewManager(path)
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	s := NewState(m)
	t.Cleanup(s.Stop)
	return s
}

// TestResolveModelPerProviderModel M1 回归：同一别名在不同供应商可指向不同真实模型，
// 每个候选必须携带自身命中的真实模型名——路由选中任意供应商时发往上游的模型名
// 必须是该供应商自己配置的名字，而非全局第一个命中的名字。
func TestResolveModelPerProviderModel(t *testing.T) {
	s := mkStateFromYML(t, `listen: {host: 127.0.0.1, port: 1}
providers:
  - {id: pa, name: A, base_url: "http://127.0.0.1:1", enabled: true, api_keys: [{id: k1, key: sk-1}], models: [{name: gpt-4o, aliases: ["4o"]}]}
  - {id: pb, name: B, base_url: "http://127.0.0.1:2", enabled: true, api_keys: [{id: k2, key: sk-2}], models: [{name: gpt-4o-mini, aliases: ["4o"]}]}
`)
	res := s.ResolveModel("4o")
	if len(res.Candidates) != 2 {
		t.Fatalf("应有 2 个候选供应商，实际 %d", len(res.Candidates))
	}
	models := map[string]string{}
	for _, c := range res.Candidates {
		models[c.Provider.Name] = c.Model
	}
	if models["A"] != "gpt-4o" || models["B"] != "gpt-4o-mini" {
		t.Fatalf("各候选应携带自身命中的真实模型名，实际 %v", models)
	}
}

// TestResolveModelAliasDedup M1 回归：同一供应商内多个模型共享同一别名时，
// 该供应商只应入候选一次（否则随机/加权策略下其权重被放大）。
func TestResolveModelAliasDedup(t *testing.T) {
	s := mkStateFromYML(t, `listen: {host: 127.0.0.1, port: 1}
providers:
  - {id: pc, name: C, base_url: "http://127.0.0.1:3", enabled: true, api_keys: [{id: k3, key: sk-3}], models: [{name: m1, aliases: ["dup"]}, {name: m2, aliases: ["dup"]}]}
`)
	res := s.ResolveModel("dup")
	if len(res.Candidates) != 1 {
		t.Fatalf("共享别名的同一供应商只应入候选一次，实际 %d", len(res.Candidates))
	}
	if res.Candidates[0].Model != "m1" {
		t.Fatalf("应取该供应商内第一个命中的模型，实际 %q", res.Candidates[0].Model)
	}
}

// TestResolveModelNamePriorityOverAlias 同供应商内模型名与另一模型的别名同名时，
// 名称匹配优先于别名匹配（取第一个命中，行为确定）。
func TestResolveModelNamePriorityOverAlias(t *testing.T) {
	s := mkStateFromYML(t, `listen: {host: 127.0.0.1, port: 1}
providers:
  - {id: pd, name: D, base_url: "http://127.0.0.1:4", enabled: true, api_keys: [{id: k4, key: sk-4}], models: [{name: real, aliases: ["x"]}, {name: x}]}
`)
	res := s.ResolveModel("x")
	if len(res.Candidates) != 1 || res.Candidates[0].Model != "real" {
		t.Fatalf("名称与别名冲突时应取第一个命中（real），实际 %+v", res.Candidates)
	}
}
