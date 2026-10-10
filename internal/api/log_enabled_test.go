package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llmproxy/internal/config"
	"llmproxy/internal/core"
	"llmproxy/internal/logsdb"
)

// TestSettingsLogEnabledRoundTrip v1.0.0 回归：日志记录开关经 PUT /api/settings
// 写入 → 快照生效 → GET 回读 → yml 持久化全链路；不带 enabled 字段的 PUT 不改动现有值。
func TestSettingsLogEnabledRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := config.NewManager(filepath.Join(dir, "cfg.yml"))
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	state := core.NewState(m)
	defer state.Stop()
	s := &Server{Mgr: m, State: state, Listen: "127.0.0.1:0"}

	getLogEnabled := func(t *testing.T) bool {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/settings", nil)
		rec := httptest.NewRecorder()
		s.handleSettingsGet(rec, req)
		if rec.Code != 200 {
			t.Fatalf("GET /api/settings status=%d body=%s", rec.Code, rec.Body.String())
		}
		var got struct {
			Log struct {
				Enabled bool   `json:"enabled"`
				DBPath  string `json:"db_path"`
			} `json:"log"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got.Log.Enabled
	}

	// 默认关闭（v1.0.0：默认不记录，旧配置升级同口径）
	if getLogEnabled(t) {
		t.Fatal("默认应关闭日志记录")
	}

	// PUT 开启 → 快照 + 回读 + yml 均生效
	put := func(body string) {
		t.Helper()
		req := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleSettingsPut(rec, req)
		if rec.Code != 200 {
			t.Fatalf("PUT /api/settings status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	put(`{"log":{"enabled":true}}`)
	if !state.Snap().LogEnabled {
		t.Fatal("PUT 开启后快照 LogEnabled 应为 true")
	}
	if !getLogEnabled(t) {
		t.Fatal("GET 回读 enabled 应为 true")
	}
	data, err := os.ReadFile(filepath.Join(dir, "cfg.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "enabled: true") {
		t.Errorf("yml 应落盘 enabled: true:\n%s", data)
	}

	// PUT 不带 enabled 字段（仅改 db_path）→ 开关状态不被改动
	put(`{"log":{"db_path":"other.db"}}`)
	if !state.Snap().LogEnabled {
		t.Fatal("不含 enabled 的 PUT 不应改动开关状态")
	}

	// PUT 关闭 → 全链路关闭
	put(`{"log":{"enabled":false}}`)
	if state.Snap().LogEnabled {
		t.Fatal("PUT 关闭后快照 LogEnabled 应为 false")
	}
	if getLogEnabled(t) {
		t.Fatal("GET 回读 enabled 应为 false")
	}
}

// TestLogsClearRecreate v1.0.0 回归：清空日志走删除数据库文件重建路径——
// 返回删除前行数、新库为空且可写、Server 引用已替换为新库。
func TestLogsClearRecreate(t *testing.T) {
	dir := t.TempDir()
	store, err := logsdb.Open(filepath.Join(dir, "log.db"))
	if err != nil {
		t.Fatal(err)
	}
	m := config.NewManager(filepath.Join(dir, "cfg.yml"))
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	state := core.NewState(m)
	defer state.Stop()
	s := &Server{Mgr: m, State: state, Listen: "127.0.0.1:0"}
	s.SetLogStore(store)
	oldStore := store // 记录旧库引用，供重建后对比

	// 模拟 main 注入的重建回调（与 main.go 逻辑一致）
	s.OnLogRecreate = func() error {
		ns, err := store.Recreate()
		if err != nil {
			return err
		}
		s.SetLogStore(ns)
		return nil
	}

	// 预置 3 条日志
	for i := 0; i < 3; i++ {
		store.WriteAsync(&logsdb.CallLog{
			ID: fmt.Sprintf("pre-%d", i), Timestamp: "2026-10-03T10:00:00+08:00",
			APIKey: "sk-test", APIKeyID: "k1", Provider: "p", RequestedModel: "m", ActualModel: "m",
			Status: "success", DurationMs: 1,
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := store.Count(); n == 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n, _ := store.Count(); n != 3 {
		t.Fatalf("预置日志应为 3 条，实际 %d", n)
	}

	// DELETE /api/logs
	req := httptest.NewRequest("DELETE", "/api/logs", nil)
	rec := httptest.NewRecorder()
	s.handleLogsClear(rec, req)
	if rec.Code != 200 {
		t.Fatalf("DELETE /api/logs status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool  `json:"ok"`
		Deleted int64 `json:"deleted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Deleted != 3 {
		t.Fatalf("应返回 ok 且 deleted=3，实际 %+v", resp)
	}

	// 新库为空且 Server 引用已替换
	ns := s.LogStore()
	if ns == oldStore {
		t.Fatal("清空后 Server 应持有重建后的新库引用")
	}
	if n, _ := ns.Count(); n != 0 {
		t.Fatalf("重建后应为空库，实际 %d 条", n)
	}
	defer ns.Close() // defer：断言 Fatal 提前退出时也确保关闭（Windows 下否则文件句柄泄漏）
	// 新库可继续写入
	ns.WriteAsync(&logsdb.CallLog{
		ID: "post-clear", Timestamp: "2026-10-03T11:00:00+08:00",
		APIKey: "sk-test", APIKeyID: "k1", Provider: "p", RequestedModel: "m", ActualModel: "m",
		Status: "success", DurationMs: 1,
	})
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := ns.Count(); n == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n, _ := ns.Count(); n != 1 {
		t.Fatalf("重建后新日志应可写入，实际 %d 条", n)
	}
}

// TestLogsClearFallback 无重建回调（测试环境）时清空降级为 SQL 删除。
func TestLogsClearFallback(t *testing.T) {
	dir := t.TempDir()
	store, err := logsdb.Open(filepath.Join(dir, "log.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.WriteAsync(&logsdb.CallLog{
		ID: "x", Timestamp: "2026-10-03T10:00:00+08:00",
		APIKey: "k", APIKeyID: "k1", Provider: "p", RequestedModel: "m", ActualModel: "m",
		Status: "success", DurationMs: 1,
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := store.Count(); n == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s := &Server{}
	s.SetLogStore(store) // 无 OnLogRecreate 回调
	req := httptest.NewRequest("DELETE", "/api/logs", nil)
	rec := httptest.NewRecorder()
	s.handleLogsClear(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Deleted int64 `json:"deleted"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Deleted != 1 {
		t.Fatalf("降级 SQL 清空应返回 deleted=1，实际 %d", resp.Deleted)
	}
	if n, _ := store.Count(); n != 0 {
		t.Fatalf("降级 SQL 清空后应为空，实际 %d", n)
	}
}
