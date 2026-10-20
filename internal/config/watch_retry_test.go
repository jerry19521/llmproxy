package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWatchRecoversWhenFixPreservesMtime 热加载对解析失败必须回滚 mtime 标记：
// 用户修复坏文件但工具保留原 mtime（如 touch -t 回填、同步工具固定时间戳）时，
// Watcher 仍应在下一轮 tick 重读并恢复；否则修复被静默忽略且无自愈路径。
// 构造要点：坏内容与好内容等长（size 相同），mtime 用 Chtimes 回填抹平——
// 两条变更线索均不触发时，唯有「解析失败回滚标记」能让下轮 tick 重新读取。
func TestWatchRecoversWhenFixPreservesMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yml")
	if err := os.WriteFile(path, []byte("listen: {port: 111}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(path)
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	m.Watch()
	t.Cleanup(m.Stop)

	// 1) 写入坏内容（与好内容等长 20 字节；mtime 变化触发解析失败）
	if err := os.WriteFile(path, []byte("listen: {port: [111\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2600 * time.Millisecond) // 等待至少一轮 tick 处理

	// 2) 修复内容（等长），并把 mtime 回填为坏文件时刻
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("listen: {port: 222}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2600 * time.Millisecond) // 等待下一轮 tick

	if got := m.Cfg().Listen.Port; got != 222 {
		t.Fatalf("mtime 未变时修复的配置应被热加载捕获，实际 port=%d", got)
	}
}
