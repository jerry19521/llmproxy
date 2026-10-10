// logsdb_test.go：日志存储测试——速率字段落库/查询、单条删除、清空。
package logsdb

import (
	"path/filepath"
	"testing"
	"time"
)

// insertSync 同步写入一条测试日志（绕过异步队列，便于断言）。
func insertSync(t *testing.T, s *LogStore, e *CallLog) {
	t.Helper()
	s.insert(e)
}

func newTestEntry(id, apikeyID, provider string, tokens int64, dur int64, rate float64) *CallLog {
	return &CallLog{
		ID: id, Timestamp: time.Now().Format(time.RFC3339),
		APIKey: "sk-test-" + apikeyID, APIKeyID: apikeyID, APIKeyName: "key-" + apikeyID,
		Provider: provider, RequestedModel: "m1", ActualModel: "m1", Status: "success",
		DurationMs: dur, GenerationMs: dur, PromptTokens: tokens / 2, CompletionTokens: tokens - tokens/2,
		TotalTokens: tokens, TokensPerSecond: rate,
	}
}

func TestStoreWriteQueryDelete(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开日志库失败: %v", err)
	}
	defer s.Close()

	insertSync(t, s, newTestEntry("id-1", "key-1", "OpenAI", 300, 1000, 300))
	insertSync(t, s, newTestEntry("id-2", "key-2", "Anthropic", 150, 3000, 50))
	insertSync(t, s, newTestEntry("id-3", "key-1", "OpenAI", 0, 200, 0))

	// 全量查询
	page, err := s.Query(LogQuery{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != 3 {
		t.Fatalf("total = %d, want 3", page.Total)
	}
	// 校验速率字段透传（同秒写入时按 id 升序，不假设顺序，遍历断言）
	found := map[string]float64{}
	for _, l := range page.Logs {
		found[l.ID] = l.TokensPerSecond
	}
	if len(found) != 3 {
		t.Fatalf("应查到 3 条不同日志，实际 %v", found)
	}
	if found["id-1"] != 300 || found["id-2"] != 50 || found["id-3"] != 0 {
		t.Fatalf("速率字段错误: %v", found)
	}

	// apikey_id 筛选
	page, err = s.Query(LogQuery{APIKeyID: "key-1"})
	if err != nil || page.Total != 2 {
		t.Fatalf("按 apikey_id 筛选失败: total=%d err=%v", page.Total, err)
	}

	// 详情
	detail, err := s.Get("id-1")
	if err != nil || detail == nil {
		t.Fatalf("查询详情失败: %v", err)
	}
	if detail.TokensPerSecond != 300 || detail.TotalTokens != 300 || detail.DurationMs != 1000 {
		t.Fatalf("详情字段错误: rate=%v total=%d dur=%d", detail.TokensPerSecond, detail.TotalTokens, detail.DurationMs)
	}
	if detail.GenerationMs != 1000 {
		t.Fatalf("详情生成耗时错误: gen=%d, want 1000", detail.GenerationMs)
	}

	// 删除单条
	n, err := s.Delete("id-1")
	if err != nil || n != 1 {
		t.Fatalf("删除单条失败: n=%d err=%v", n, err)
	}
	page, _ = s.Query(LogQuery{})
	if page.Total != 2 {
		t.Fatalf("删除后 total = %d, want 2", page.Total)
	}
	// 删除不存在的 id
	n, err = s.Delete("not-exist")
	if err != nil || n != 0 {
		t.Fatalf("删除不存在 id 应影响 0 行: n=%d err=%v", n, err)
	}

	// 清空全部
	n, err = s.DeleteAll()
	if err != nil || n != 2 {
		t.Fatalf("清空失败: n=%d err=%v", n, err)
	}
	page, _ = s.Query(LogQuery{})
	if page.Total != 0 {
		t.Fatalf("清空后 total = %d, want 0", page.Total)
	}
	// 空库再清空
	n, err = s.DeleteAll()
	if err != nil || n != 0 {
		t.Fatalf("空库清空应影响 0 行: n=%d err=%v", n, err)
	}
}
