package core

import (
	"fmt"
	"testing"
)

// TestPickProviderRRCounterBounded 轮询计数表只增不删（键为模型名，含别名、
// 任意试探输入），必须设上限防御异常调用方无限刷键导致内存无界增长：
// 达到上限时整体重建，轮询位置重置仅影响该极端场景，正常模型规模永远不触发。
func TestPickProviderRRCounterBounded(t *testing.T) {
	s := mkStateFromYML(t, `listen: {host: 127.0.0.1, port: 1}
providers:
  - {id: pa, name: A, base_url: "http://127.0.0.1:1", enabled: true, api_keys: [{id: k1, key: sk-1, enabled: true}], models: [{name: m1}]}
`)
	res := s.ResolveModel("m1")
	if len(res.Candidates) == 0 {
		t.Fatal("应有候选供应商")
	}
	const limit = 4096
	for i := 0; i < limit+500; i++ {
		s.PickProvider(res.Candidates, "round_robin", fmt.Sprintf("alias-%d", i))
	}
	s.rrMu.Lock()
	n := len(s.rrCounter)
	s.rrMu.Unlock()
	if n > limit {
		t.Fatalf("轮询计数表应受 %d 键上限约束，实际 %d 个键", limit, n)
	}
}
