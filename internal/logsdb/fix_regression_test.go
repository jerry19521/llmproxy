package logsdb

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestWriteAsyncCloseNoPanic H1 回归：WriteAsync 与 Close 并发交错不得 panic。
// 旧实现「锁内读 closed + 锁外 send」对「锁内置位 + 锁外 close(ch)」存在固有竞态
// （send on closed channel panic，select+default 无法规避），服务优雅关闭/
// 日志库热切换时几乎每轮可复现；v1.0.0 改为 quit/done 信号对 + 锁内 send。
func TestWriteAsyncCloseNoPanic(t *testing.T) {
	db := filepath.Join(t.TempDir(), "h1.db")
	const rounds = 10
	for r := 0; r < rounds; r++ {
		store, err := Open(db)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var panics int32
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if rec := recover(); rec != nil {
						atomic.AddInt32(&panics, 1)
					}
				}()
				for i := 0; i < 2000; i++ {
					store.WriteAsync(&CallLog{ID: "x", Timestamp: "2026-01-01T00:00:00Z",
						APIKey: "k", APIKeyID: "k", Provider: "p", RequestedModel: "m", ActualModel: "m",
						Status: "success", DurationMs: 1})
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(200 * time.Microsecond) // 与写入高峰交错
			_ = store.Close()
		}()
		wg.Wait()
		if atomic.LoadInt32(&panics) > 0 {
			t.Fatalf("WriteAsync/Close 交错不应 panic（旧竞态复现），round=%d panics=%d", r, panics)
		}
	}
}

// TestCloseDrainsQueuedEntries 关闭时已入队条目应全部落库（优雅关闭不丢日志）。
func TestCloseDrainsQueuedEntries(t *testing.T) {
	db := filepath.Join(t.TempDir(), "drain.db")
	store, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	const n = 500
	for i := 0; i < n; i++ {
		store.WriteAsync(&CallLog{ID: fmt.Sprintf("e-%d", i), Timestamp: "2026-01-01T00:00:00Z",
			APIKey: "k", APIKeyID: "k", Provider: "p", RequestedModel: "m", ActualModel: "m",
			Status: "success", DurationMs: 1})
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// 重新打开验证排空落库
	store2, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	page, err := store2.Query(LogQuery{Page: 1, PageSize: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(page.Logs)) < n {
		t.Fatalf("关闭前已入队的 %d 条日志应全部落库，实际 %d", n, len(page.Logs))
	}
}

// TestKeywordLikeEscaping L3 回归：keyword 中的 % 与 _ 应按字面匹配而非被当作通配符。
func TestKeywordLikeEscaping(t *testing.T) {
	db := filepath.Join(t.TempDir(), "kw.db")
	store, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.WriteAsync(&CallLog{ID: "a", Timestamp: "2026-01-01T00:00:00Z", APIKey: "sk-abc",
		APIKeyID: "k", Provider: "prov-1", RequestedModel: "m", ActualModel: "m", Status: "success", DurationMs: 1})
	// 等异步写入完成
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p, _ := store.Query(LogQuery{Page: 1, PageSize: 10})
		if p != nil && p.Total > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// "100%" 只应匹配含字面 "100%" 的记录，不应把 % 当任意后缀通配
	p, err := store.Query(LogQuery{Page: 1, PageSize: 10, Keyword: "prov-1%"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 0 {
		t.Fatalf("含通配符的 keyword 应按字面匹配（prov-1%% 不应命中 prov-1），实际 %d", p.Total)
	}
	p, err = store.Query(LogQuery{Page: 1, PageSize: 10, Keyword: "prov-"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 1 {
		t.Fatalf("普通前缀 keyword 应命中 1 条，实际 %d", p.Total)
	}
}
