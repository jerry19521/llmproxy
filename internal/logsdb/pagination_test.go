package logsdb

// 回归测试集：
//   S3  超大 Page 值被 clamp，offset 不溢出、分页元数据不荒谬
//   M8  分页 total 由窗口函数随主查询一遍扫描得出（含 keyword 条件场景）

import (
	"path/filepath"
	"testing"
	"time"
)

func TestQueryHugePageClamped(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开日志库失败: %v", err)
	}
	defer s.Close()
	for i := 0; i < 3; i++ {
		insertSync(t, s, &CallLog{
			ID:        time.Now().Format("150405.000000000") + "-" + string(rune('a'+i)),
			Timestamp: time.Now().Format(time.RFC3339),
			APIKey:    "sk-t", APIKeyID: "k1", Provider: "P", RequestedModel: "m", ActualModel: "m",
			Status: "success",
		})
	}
	// 超大页码：应被 clamp 到上限，offset 计算不回绕，查询正常返回（空页但 total 正确）
	page, err := s.Query(LogQuery{Page: 1 << 40, PageSize: 500})
	if err != nil {
		t.Fatalf("超大页码查询不应报错: %v", err)
	}
	if page.Page != 100000 {
		t.Fatalf("超大页码应 clamp 到 100000，实际 %d", page.Page)
	}
	if page.Total != 3 {
		t.Fatalf("total 应为 3，实际 %d", page.Total)
	}
	if len(page.Logs) != 0 {
		t.Fatalf("offset 远超总数应返回空页，实际 %d 条", len(page.Logs))
	}
}

func TestQueryTotalWithKeyword(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开日志库失败: %v", err)
	}
	defer s.Close()
	insertSync(t, s, &CallLog{
		ID: "k-a", Timestamp: time.Now().Format(time.RFC3339),
		APIKey: "sk-t", APIKeyID: "k1", Provider: "OpenAI", RequestedModel: "gpt", ActualModel: "gpt",
		Status: "success", ReqBody: "你好世界",
	})
	insertSync(t, s, &CallLog{
		ID: "k-b", Timestamp: time.Now().Format(time.RFC3339),
		APIKey: "sk-t", APIKeyID: "k1", Provider: "Other", RequestedModel: "m2", ActualModel: "m2",
		Status: "success", ReqBody: "无关内容",
	})
	// keyword 命中（total 与数据一遍扫描，窗口函数在 LIMIT 前计算）
	page, err := s.Query(LogQuery{Keyword: "你好"})
	if err != nil {
		t.Fatalf("keyword 查询失败: %v", err)
	}
	if page.Total != 1 || len(page.Logs) != 1 || page.Logs[0].ID != "k-a" {
		t.Fatalf("keyword 命中应 1 条（total=%d, len=%d）", page.Total, len(page.Logs))
	}
	// keyword 不命中：空结果 total=0
	page, err = s.Query(LogQuery{Keyword: "不存在的关键词"})
	if err != nil {
		t.Fatalf("keyword 查询失败: %v", err)
	}
	if page.Total != 0 || len(page.Logs) != 0 {
		t.Fatalf("不命中应空结果（total=%d, len=%d）", page.Total, len(page.Logs))
	}
}
