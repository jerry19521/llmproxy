package logsdb

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitCount 轮询等待日志总行数达到 want（异步写入落库有延迟）。
func waitCount(t *testing.T, s *LogStore, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, err := s.Count()
		if err != nil {
			t.Fatalf("Count 失败: %v", err)
		}
		if n >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	n, _ := s.Count()
	t.Fatalf("等待 %d 条日志超时，当前 %d 条", want, n)
}

func totalSize(paths ...string) int64 {
	var sum int64
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			sum += fi.Size()
		}
	}
	return sum
}

// TestCountAndRecreate v1.0.0 回归：Count 行数统计 + Recreate 删除库文件重建——
// 新库为空且可继续写入、旧数据全部消失、磁盘空间实际释放（重建后文件远小于重建前）。
func TestCountAndRecreate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	// 写入 60 条带 64KB 大 body 的日志（约 3.9MB，制造膨胀文件）
	for i := 0; i < 60; i++ {
		s.WriteAsync(&CallLog{
			ID: fmt.Sprintf("id-%03d", i), Timestamp: "2026-10-03T10:00:00+08:00",
			APIKey: "sk-test", APIKeyID: "k1", Provider: "p", RequestedModel: "m", ActualModel: "m",
			Status: "success", DurationMs: 10, ReqBody: strings.Repeat("x", 64*1024),
		})
	}
	waitCount(t, s, 60)

	// Count 精确性
	if n, _ := s.Count(); n != 60 {
		t.Fatalf("Count 应为 60，实际 %d", n)
	}

	// 关闭旧库触发 WAL checkpoint：数据合并进主文件，得到膨胀后的真实大小
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	oldSize := totalSize(path, path+"-wal", path+"-shm")
	if oldSize < 1024*1024 {
		t.Fatalf("写入 60×64KB 后库文件应明显膨胀（>1MB），实际 %d 字节", oldSize)
	}

	// Recreate：Close 幂等 + 删除文件 + 原路径重建空库
	ns, err := s.Recreate()
	if err != nil {
		t.Fatalf("Recreate 失败: %v", err)
	}
	defer ns.Close()

	// 新库为空
	if n, _ := ns.Count(); n != 0 {
		t.Fatalf("重建后应为空库，实际 %d 条", n)
	}
	// 旧详情不可查（数据已随文件删除）
	if e, _ := ns.Get("id-000"); e != nil {
		t.Fatalf("重建后旧日志不应可查")
	}
	// 磁盘空间已释放：新库远小于旧库
	newSize := totalSize(path, path+"-wal", path+"-shm")
	if newSize >= oldSize {
		t.Fatalf("重建后文件应显著小于重建前: old=%d new=%d", oldSize, newSize)
	}
	// 新库可正常写入与查询
	ns.WriteAsync(&CallLog{
		ID: "after-recreate", Timestamp: "2026-10-03T11:00:00+08:00",
		APIKey: "sk-test", APIKeyID: "k1", Provider: "p", RequestedModel: "m", ActualModel: "m",
		Status: "success", DurationMs: 5,
	})
	waitCount(t, ns, 1)
	if e, _ := ns.Get("after-recreate"); e == nil || e.ID != "after-recreate" {
		t.Fatalf("重建后新日志写入/查询异常: %+v", e)
	}
	// Path 返回原路径
	if ns.Path() != path {
		t.Fatalf("重建后路径应不变: %q", ns.Path())
	}
}

// TestRecreateOnFreshStore 空库直接重建（清空按钮在无日志时点击）不报错。
func TestRecreateOnFreshStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ns, err := s.Recreate()
	if err != nil {
		t.Fatalf("空库 Recreate 失败: %v", err)
	}
	defer ns.Close()
	if n, _ := ns.Count(); n != 0 {
		t.Fatalf("空库重建后应仍为空，实际 %d", n)
	}
}

// TestRecreateConcurrentCloseSafety Recreate 期间并发 WriteAsync 不 panic
// （旧库 closed 后 WriteAsync 静默丢弃，H1 语义保持）。
func TestRecreateConcurrentCloseSafety(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			s.WriteAsync(&CallLog{
				ID: fmt.Sprintf("c-%d", i), Timestamp: "2026-10-03T12:00:00+08:00",
				APIKey: "k", APIKeyID: "k1", Provider: "p", RequestedModel: "m", ActualModel: "m",
				Status: "success", DurationMs: 1,
			})
		}
	}()
	ns, err := s.Recreate()
	if err != nil {
		t.Fatalf("并发写入下 Recreate 失败: %v", err)
	}
	defer ns.Close()
	<-done
}
