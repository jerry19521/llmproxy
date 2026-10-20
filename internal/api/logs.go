package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llmproxy/internal/core"
	"llmproxy/internal/logsdb"
)

// isClosedDBErr 判定错误是否为「日志库已被关闭」：清空重建 / 路径切换 / 热加载切换
// 关闭旧库后的窗口内，持有旧引用的查询会收到 sql: database is closed 类错误。
// 作为读接口「重取引用重试一次」兑底的触发依据。
func isClosedDBErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "database is closed")
}

func (s *Server) handleLogsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	pageSize, _ := strconv.Atoi(q.Get("page_size"))
	// 无法解析的时间参数返回 400——原样透传进 SQL 字符串比较，
	// start_time 非法会恒假过滤掉全部日志（用户误以为没日志）、end_time 非法恒真等于没过滤。
	startTime, okS := normalizeTimeParam(q.Get("start_time"), false)
	if !okS {
		writeErr(w, http.StatusBadRequest, "start_time 需为 ISO 8601 时间（如 2026-01-01T10:00:00 或 2026-01-01）", "invalid_request")
		return
	}
	endTime, okE := normalizeTimeParam(q.Get("end_time"), true)
	if !okE {
		writeErr(w, http.StatusBadRequest, "end_time 需为 ISO 8601 时间（如 2026-01-01T10:00:00 或 2026-01-01）", "invalid_request")
		return
	}
	query := logsdb.LogQuery{
		Page:      page,
		PageSize:  pageSize,
		Status:    q.Get("status"),
		Provider:  q.Get("provider"),
		Model:     q.Get("model"),
		APIKeyID:  q.Get("apikey_id"),
		Keyword:   q.Get("keyword"),
		StartTime: startTime,
		EndTime:   endTime,
	}
	// store 引用在 logOpMu 读锁内获取；若在锁外获取，清空重建窗口内阻塞在
	// RLock 的查询线程醒来后拿到的仍是已关旧库，会直接返回 500。
	s.logOpMu.RLock()
	store := s.LogStore()
	if store == nil {
		s.logOpMu.RUnlock()
		writeErr(w, http.StatusInternalServerError, "日志数据库不可用", "internal_error")
		return
	}
	result, err := store.Query(query)
	s.logOpMu.RUnlock()
	// 遇 closed 错误时重取引用重试一次（判定 st != store：引用未变则不
	// 重试，避免无意义重复查询）。三条切换路径（设置页 / 清空 / 热加载
	// SwitchLogPathLocked）全部持 logOpMu，此处重试为防御性兜底。
	if err != nil && isClosedDBErr(err) {
		s.logOpMu.RLock()
		if st := s.LogStore(); st != nil && st != store {
			result, err = st.Query(query)
		}
		s.logOpMu.RUnlock()
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询失败: "+err.Error(), "internal_error")
		return
	}
	maskLogAPIKeys(result.Logs)
	writeJSON(w, http.StatusOK, result)
}

// maskLogAPIKeys 日志列表 API Key 脱敏（前 3 位 + *** + 后 3 位，与 API Key 下拉框一致）。
// 列表页用于展示即可节省空间并降低泄露面；详情接口保留完整 Key。
func maskLogAPIKeys(logs []*logsdb.CallLog) {
	for _, l := range logs {
		if l.APIKey != "" {
			l.APIKey = core.MaskKey(l.APIKey)
		}
	}
}

func (s *Server) handleLogDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// store 引用在锁内获取（同 handleLogsList）+ closed 重试
	s.logOpMu.RLock()
	store := s.LogStore()
	if store == nil {
		s.logOpMu.RUnlock()
		writeErr(w, http.StatusInternalServerError, "日志数据库不可用", "internal_error")
		return
	}
	entry, err := store.Get(id)
	s.logOpMu.RUnlock()
	if err != nil && isClosedDBErr(err) {
		s.logOpMu.RLock()
		if st := s.LogStore(); st != nil && st != store {
			entry, err = st.Get(id)
		}
		s.logOpMu.RUnlock()
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询失败: "+err.Error(), "internal_error")
		return
	}
	if entry == nil {
		writeErr(w, http.StatusNotFound, "log not found", "not_found")
		return
	}
	// 附带解析后的尝试记录
	resp := map[string]interface{}{}
	b, _ := json.Marshal(entry)
	_ = json.Unmarshal(b, &resp)
	if entry.AttemptLogsJSON != "" {
		var attempts []logsdb.AttemptLog
		if err := json.Unmarshal([]byte(entry.AttemptLogsJSON), &attempts); err == nil {
			resp["attempt_logs"] = attempts
		} else {
			resp["attempt_logs"] = entry.AttemptLogsJSON
		}
	} else {
		resp["attempt_logs"] = []logsdb.AttemptLog{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleLogDelete 删除单条日志。
func (s *Server) handleLogDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// store 引用在锁内获取 + closed 重试。DELETE 幂等：旧库被关时
	// 本次删除必然未生效，重试对新库执行一次，不存在重复删除。
	s.logOpMu.RLock()
	store := s.LogStore()
	if store == nil {
		s.logOpMu.RUnlock()
		writeErr(w, http.StatusInternalServerError, "日志数据库不可用", "internal_error")
		return
	}
	n, err := store.Delete(id)
	s.logOpMu.RUnlock()
	if err != nil && isClosedDBErr(err) {
		s.logOpMu.RLock()
		if st := s.LogStore(); st != nil && st != store {
			n, err = st.Delete(id)
		}
		s.logOpMu.RUnlock()
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "删除失败: "+err.Error(), "internal_error")
		return
	}
	if n == 0 {
		writeErr(w, http.StatusNotFound, "log not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "deleted": n})
}

// handleLogsClear 清空全部日志（删除数据库文件并重建，即时释放磁盘空间）。
// 行数统计在重建前完成；OnLogRecreate 回调（main 注入）负责关库、删除库文件与
// -wal/-shm 伴生文件、原路径重建空库并替换引用；无回调（测试环境）降级为 SQL 删除。
// logOpMu 互斥：防止并发清空（双击/多管理员）交错重建互相删掉对方的新库。
func (s *Server) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	s.logOpMu.Lock()
	defer s.logOpMu.Unlock()
	if s.OnLogRecreate != nil {
		// store 改为锁内捕获——在锁外捕获，并发双清空时后进锁的请求
		// 拿到已被重建关闭的旧 store：Count() 报 database is closed 被吞、响应 deleted:0；
		// 锁内捕获保证拿到重建后的新 store（main 的 onSwap 已同步替换引用）。
		store := s.LogStore()
		if store == nil {
			writeErr(w, http.StatusInternalServerError, "日志数据库不可用", "internal_error")
			return
		}
		// 重建前统计行数（尽力而为：统计与重建间新写入的日志随文件一并删除）
		n, _ := store.Count()
		if err := s.OnLogRecreate(); err != nil {
			writeErr(w, http.StatusInternalServerError, "清空失败: "+err.Error(), "internal_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "deleted": n})
		return
	}
	// 无回调（测试环境）：降级为 SQL 清空（store 同样锁内捕获，与重建路径同口径）
	store := s.LogStore()
	if store == nil {
		writeErr(w, http.StatusInternalServerError, "日志数据库不可用", "internal_error")
		return
	}
	n, err := store.DeleteAll()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "删除失败: "+err.Error(), "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "deleted": n})
}

// normalizeTimeParam 时间参数归一化为本地时区 RFC3339（兼容 Z 结尾与空格分隔）。
// 输出统一转为本地时区：日志 timestamp 按本地 RFC3339 存储，SQL 为字符串比较，
// 若保留 UTC（Z 后缀）表示，字典序与时间序不一致会导致时间范围筛选错乱。
// isEnd：作为结束时间时，纯日期输入补全为当天 23:59:59（——统一补 00:00:00，
// 直接调 API 传纯日期 end_time 会漏掉当天全部日志；与 expires_at 的补全口径对齐）。
// 返回值增加 ok——所有 layout 均不匹配时返回 false，调用方据此 400 拒绝，
// 不再原样透传进 SQL 字符串比较（非法 start_time 恒假过滤全部、非法 end_time 恒真等于没过滤）。
func normalizeTimeParam(s string, isEnd bool) (string, bool) {
	if s == "" {
		return "", true
	}
	layouts := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"}
	for _, layout := range layouts {
		var t time.Time
		var err error
		if layout == time.RFC3339Nano || layout == time.RFC3339 {
			t, err = time.Parse(layout, s)
		} else {
			t, err = time.ParseInLocation(layout, s, time.Local)
		}
		if err == nil {
			if isEnd && layout == "2006-01-02" {
				t = t.Add(23*time.Hour + 59*time.Minute + 59*time.Second)
			}
			return t.Local().Format(time.RFC3339), true
		}
	}
	return s, false
}
