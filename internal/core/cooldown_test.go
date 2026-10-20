package core

import (
	"testing"
	"time"

	"llmproxy/internal/config"
)

// TestCooldownAddSeconds 冷却池以秒为单位计时。
func TestCooldownAddSeconds(t *testing.T) {
	p := NewCooldownPool()
	p.Add("p1", "m1", 30)
	if !p.IsCooling("p1", "m1") {
		t.Fatal("入池后应处于冷却中")
	}
	p.mu.Lock()
	until := p.items[cooldownKey("p1", "m1")]
	p.mu.Unlock()
	remain := time.Until(until)
	if remain <= 25*time.Second || remain > 31*time.Second {
		t.Errorf("冷却剩余应约 30 秒, got %v", remain)
	}

	// 非法值兜底 300 秒
	p.Add("p2", "m2", 0)
	p.mu.Lock()
	until2 := p.items[cooldownKey("p2", "m2")]
	p.mu.Unlock()
	if r := time.Until(until2); r <= 290*time.Second || r > 301*time.Second {
		t.Errorf("seconds<=0 应兜底 300 秒, got %v", r)
	}
}

// TestRecordFailureThreshold 重试次数阈值：连续 N 轮全部 Key 失败后才入池。
func TestRecordFailureThreshold(t *testing.T) {
	p := NewCooldownPool()
	// 前 2 轮：未达阈值（3），不入池
	for i := 1; i <= 2; i++ {
		if cooled := p.RecordFailure("p1", "m1", 60, 3); cooled {
			t.Fatalf("第 %d 轮失败不应入池", i)
		}
		if p.IsCooling("p1", "m1") {
			t.Fatalf("第 %d 轮失败不应处于冷却中", i)
		}
	}
	// 第 3 轮：达到阈值，入池
	if cooled := p.RecordFailure("p1", "m1", 60, 3); !cooled {
		t.Fatal("第 3 轮失败应入池")
	}
	if !p.IsCooling("p1", "m1") {
		t.Fatal("达到阈值后应处于冷却中")
	}
	p.mu.Lock()
	remain := time.Until(p.items[cooldownKey("p1", "m1")])
	p.mu.Unlock()
	if r := remain; r <= 55*time.Second || r > 61*time.Second {
		t.Errorf("冷却时长应约 60 秒, got %v", r)
	}
	// 入池后失败计数清零
	p.mu.Lock()
	_, streakLeft := p.failStreak[cooldownKey("p1", "m1")]
	p.mu.Unlock()
	if streakLeft {
		t.Error("入池后连续失败计数应清零")
	}
}

// TestRecordFailureImmediate 阈值为 1 时一轮失败立即冷却（保持阈值最小为 1 时即失效的兜底语义）。
func TestRecordFailureImmediate(t *testing.T) {
	p := NewCooldownPool()
	if cooled := p.RecordFailure("p1", "m1", 60, 1); !cooled {
		t.Fatal("threshold=1 时一轮失败即应入池")
	}
	if !p.IsCooling("p1", "m1") {
		t.Fatal("threshold=1 时入池后应冷却中")
	}
}

// TestRecordSuccessResetsStreak 成功调用清零连续失败计数。
func TestRecordSuccessResetsStreak(t *testing.T) {
	p := NewCooldownPool()
	p.RecordFailure("p1", "m1", 60, 3)
	p.RecordFailure("p1", "m1", 60, 3)
	p.RecordSuccess("p1", "m1")
	// 清零后需重新累计 3 轮才入池
	if cooled := p.RecordFailure("p1", "m1", 60, 3); cooled {
		t.Fatal("成功清零后 1 轮失败不应入池")
	}
	if cooled := p.RecordFailure("p1", "m1", 60, 3); cooled {
		t.Fatal("成功清零后 2 轮失败不应入池")
	}
	if cooled := p.RecordFailure("p1", "m1", 60, 3); !cooled {
		t.Fatal("成功清零后第 3 轮失败应入池")
	}
}

// TestRemoveClearsStreak 手动移除条目同时清零失败计数。
func TestRemoveClearsStreak(t *testing.T) {
	p := NewCooldownPool()
	p.RecordFailure("p1", "m1", 60, 3)
	p.RecordFailure("p1", "m1", 60, 3)
	p.Remove("p1", "m1")
	if cooled := p.RecordFailure("p1", "m1", 60, 3); cooled {
		t.Fatal("Remove 清零后 1 轮失败不应入池")
	}
	if got := mustStreak(t, p, "p1", "m1"); got != 1 {
		t.Errorf("Remove 后失败计数应从 0 重新累计, got %d", got)
	}
}

// TestClearClearsStreak 清空冷却池同时清零全部失败计数。
func TestClearClearsStreak(t *testing.T) {
	p := NewCooldownPool()
	p.RecordFailure("p1", "m1", 60, 3)
	p.RecordFailure("p2", "m2", 60, 1)
	p.Clear()
	if mustStreak(t, p, "p1", "m1") != 0 || p.IsCooling("p2", "m2") {
		t.Fatal("Clear 应清空条目与失败计数")
	}
}

// TestFilterCooldownDisabled 冷却池禁用时过滤直通。
func TestFilterCooldownDisabled(t *testing.T) {
	dir := t.TempDir()
	m := config.NewManager(dir + "/cfg.yml")
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	s := NewState(m)
	defer s.Stop()

	cands := []ProviderCandidate{{Provider: &config.Provider{ID: "p1", Name: "P1"}, Model: "m1"}}
	s.Cooldown.Add("p1", "m1", 60)

	// 启用（默认）：应过滤掉冷却中的供应商
	if got := s.FilterCooldown(cands); len(got) != 0 {
		t.Errorf("启用冷却池时应过滤冷却条目, got %d", len(got))
	}

	// 禁用：原样返回
	if err := s.MutateSettings(func(ns *Snapshot) { ns.CooldownEnabled = false }); err != nil {
		t.Fatal(err)
	}
	got := s.FilterCooldown(cands)
	if len(got) != 1 || got[0].Provider.ID != "p1" {
		t.Errorf("禁用冷却池时应原样返回候选, got %v", got)
	}
}

// mustStreak 读取连续失败计数（测试辅助）。
func mustStreak(t *testing.T, p *CooldownPool, providerID, model string) int {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failStreak[cooldownKey(providerID, model)]
}
