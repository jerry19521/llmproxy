// Package logsdb 调用日志的 SQLite 存储：异步写入、分页查询、详情、统计聚合。
package logsdb

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// AttemptLog 单次上游尝试记录。
type AttemptLog struct {
	Provider string `json:"provider"`
	APIKeyID string `json:"api_key_id"`
	ErrorMsg string `json:"error_msg"`
}

// CallLog 调用日志记录。
// 请求/响应按传输方向分四组记录，排查时可完整还原链路：
//
//	客户端 → 代理：req_method / client_req_url / req_headers / req_body
//	代理 → 上游：upstream_url / upstream_req_headers / upstream_req_body（认证头脱敏）
//	上游 → 代理：resp_status / resp_headers / resp_body（上游响应体副本）
//	代理 → 客户端：client_resp_status / client_resp_headers / client_resp_body
//
// 兼容说明：旧版本 req_url 记录的是上游 URL（现改由 upstream_url 承担，新数据不再写 req_url）；
// 旧版本流式场景 resp_body 被覆盖为发给客户端的字节（现由 client_resp_body 承担，
// resp_body 统一为上游响应体）。
type CallLog struct {
	ID               string       `json:"id"`
	Timestamp        string       `json:"timestamp"`
	APIKey           string       `json:"apikey"`
	APIKeyID         string       `json:"apikey_id"`
	APIKeyName       string       `json:"apikey_name"`
	RetryCount       int          `json:"retry_count"`
	AttemptLogs      []AttemptLog `json:"-"`
	AttemptLogsJSON  string       `json:"-"`
	Provider         string       `json:"provider"`
	RequestedModel   string       `json:"requested_model"`
	ActualModel      string       `json:"actual_model"`
	Status           string       `json:"status"`
	ErrorMsg         *string      `json:"error_msg"`
	DurationMs       int64        `json:"duration_ms"`
	GenerationMs     int64        `json:"generation_ms"`
	PromptTokens     int64        `json:"prompt_tokens"`
	CompletionTokens int64        `json:"completion_tokens"`
	TotalTokens      int64        `json:"total_tokens"`
	TokensPerSecond  float64      `json:"tokens_per_second"`
	ReqURL           string       `json:"req_url,omitempty"` // 兼容旧数据：旧版记录上游 URL，新数据为空，改用 UpstreamURL
	ReqMethod        string       `json:"req_method,omitempty"`
	ReqHeaders       string       `json:"req_headers,omitempty"`
	ReqBody          string       `json:"req_body,omitempty"`
	// 客户端请求路径（客户端打到代理的 URL，如 /v1/responses）。
	ClientReqURL string `json:"client_req_url,omitempty"`
	// 代理 → 上游请求（四组数据之二；认证头脱敏后记录）。
	UpstreamURL        string `json:"upstream_url,omitempty"`
	UpstreamReqHeaders string `json:"upstream_req_headers,omitempty"`
	UpstreamReqBody    string `json:"upstream_req_body,omitempty"`
	RespStatus         int    `json:"resp_status,omitempty"`
	RespHeaders        string `json:"resp_headers,omitempty"`
	RespBody           string `json:"resp_body,omitempty"`
	// 代理 → 客户端响应（四组数据之四；流式为已发送部分，跨格式为转换后输出）。
	ClientRespStatus  int    `json:"client_resp_status,omitempty"`
	ClientRespHeaders string `json:"client_resp_headers,omitempty"`
	ClientRespBody    string `json:"client_resp_body,omitempty"`
	// v1.0.0 速率修复新增：用于 reasoning 流等场景的速率/可观测性。
	//   FirstByteMs：上游首个响应字节到达时刻（请求发出起算）——流式 token 速率的起算点；
	//   FirstContentMs：首个 content 类增量到达时刻（请求起算）；
	//   LastChunkMs：末字节时刻（与 DurationMs 通常一致，记录下来便于审计）；
	//   SentBytes：实际成功写入客户端的字节数；
	//   LastSentMs：最后一次成功写入客户端的时刻。
	FirstByteMs    int64 `json:"first_byte_ms,omitempty"`
	FirstContentMs int64 `json:"first_content_ms,omitempty"`
	LastChunkMs    int64 `json:"last_chunk_ms,omitempty"`
	SentBytes      int64 `json:"sent_bytes,omitempty"`
	LastSentMs     int64 `json:"last_sent_ms,omitempty"`
}

// LogStore SQLite 日志存储。
type LogStore struct {
	mu      sync.Mutex
	db      *sql.DB
	path    string
	ch      chan *CallLog
	quit    chan struct{} // 关闭信号：通知 writer 停止接收新写入
	done    chan struct{} // writer 排空退出信号
	closed  bool
	dropped int64 // 队列满丢弃计数（原子操作：多 goroutine 并发 WriteAsync 竞争递增）
	// writeFailures 写入失败计数（v1.0.0 S7：磁盘满/库损坏期间静默丢失的条数，
	// 原子操作；Close 时汇总打印，保持日志子系统的可观测性）
	writeFailures int64
}

const schema = `
CREATE TABLE IF NOT EXISTS call_logs (
  id TEXT PRIMARY KEY,
  timestamp TEXT NOT NULL,
  apikey TEXT NOT NULL,
  apikey_id TEXT NOT NULL,
  apikey_name TEXT,
  retry_count INTEGER DEFAULT 0,
  attempt_logs TEXT,
  provider TEXT NOT NULL,
  requested_model TEXT NOT NULL,
  actual_model TEXT NOT NULL,
  status TEXT NOT NULL,
  error_msg TEXT,
  duration_ms INTEGER NOT NULL,
  generation_ms INTEGER DEFAULT 0,
  prompt_tokens INTEGER,
  completion_tokens INTEGER,
  total_tokens INTEGER,
  tokens_per_second REAL DEFAULT 0,
  req_url TEXT,
  req_method TEXT,
  req_headers TEXT,
  req_body TEXT,
  client_req_url TEXT,
  upstream_url TEXT,
  upstream_req_headers TEXT,
  upstream_req_body TEXT,
  resp_status INTEGER,
  resp_headers TEXT,
  resp_body TEXT,
  client_resp_status INTEGER,
  client_resp_headers TEXT,
  client_resp_body TEXT,
  first_byte_ms INTEGER DEFAULT 0,
  first_content_ms INTEGER DEFAULT 0,
  last_chunk_ms INTEGER DEFAULT 0,
  sent_bytes INTEGER DEFAULT 0,
  last_sent_ms INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_logs_timestamp ON call_logs(timestamp);
CREATE INDEX IF NOT EXISTS idx_logs_apikey ON call_logs(apikey);
CREATE INDEX IF NOT EXISTS idx_logs_provider ON call_logs(provider);
CREATE INDEX IF NOT EXISTS idx_logs_requested_model ON call_logs(requested_model);
CREATE INDEX IF NOT EXISTS idx_logs_actual_model ON call_logs(actual_model);
`

// Open 打开（或创建）日志数据库并启动异步写入。
func Open(path string) (*LogStore, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建日志目录失败: %w", err)
		}
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite 单写连接，避免锁冲突
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化日志表失败: %w", err)
	}
	// 旧库迁移：v1.3.0 之前的库无 tokens_per_second 列，v1.5.0 之前的库无 generation_ms 列，
	// v1.7.1 之前的库无 first_content_ms/last_chunk_ms/sent_bytes/last_sent_ms 列，
	// v1.7.5 之前的库无四组请求/响应详情列与 first_byte_ms 列。
	// v1.0.0（S5）：预查 PRAGMA table_info 决定是否执行 ALTER——旧实现靠错误消息文本
	// "duplicate column" 判重，依赖 modernc.org/sqlite 当前版本的错误文案；该文案
	// 非 API 契约，依赖库升级或本地化时会误判为致命错误导致启动失败。
	existing := map[string]bool{}
	if rows, err := db.Query(`PRAGMA table_info(call_logs)`); err == nil {
		for rows.Next() {
			var cid, notnull, pk int
			var name, ctype string
			var dflt sql.NullString
			if rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk) == nil {
				existing[name] = true
			}
		}
		rows.Close()
	}
	for _, m := range []struct{ col, ddl string }{
		{"tokens_per_second", `ALTER TABLE call_logs ADD COLUMN tokens_per_second REAL DEFAULT 0`},
		{"generation_ms", `ALTER TABLE call_logs ADD COLUMN generation_ms INTEGER DEFAULT 0`},
		{"first_byte_ms", `ALTER TABLE call_logs ADD COLUMN first_byte_ms INTEGER DEFAULT 0`},
		{"first_content_ms", `ALTER TABLE call_logs ADD COLUMN first_content_ms INTEGER DEFAULT 0`},
		{"last_chunk_ms", `ALTER TABLE call_logs ADD COLUMN last_chunk_ms INTEGER DEFAULT 0`},
		{"sent_bytes", `ALTER TABLE call_logs ADD COLUMN sent_bytes INTEGER DEFAULT 0`},
		{"last_sent_ms", `ALTER TABLE call_logs ADD COLUMN last_sent_ms INTEGER DEFAULT 0`},
		{"client_req_url", `ALTER TABLE call_logs ADD COLUMN client_req_url TEXT`},
		{"upstream_url", `ALTER TABLE call_logs ADD COLUMN upstream_url TEXT`},
		{"upstream_req_headers", `ALTER TABLE call_logs ADD COLUMN upstream_req_headers TEXT`},
		{"upstream_req_body", `ALTER TABLE call_logs ADD COLUMN upstream_req_body TEXT`},
		{"client_resp_status", `ALTER TABLE call_logs ADD COLUMN client_resp_status INTEGER`},
		{"client_resp_headers", `ALTER TABLE call_logs ADD COLUMN client_resp_headers TEXT`},
		{"client_resp_body", `ALTER TABLE call_logs ADD COLUMN client_resp_body TEXT`},
	} {
		if existing[m.col] {
			continue
		}
		if _, err := db.Exec(m.ddl); err != nil {
			db.Close()
			return nil, fmt.Errorf("迁移日志表失败: %w", err)
		}
	}
	s := &LogStore{db: db, path: path, ch: make(chan *CallLog, 2048), quit: make(chan struct{}), done: make(chan struct{})}
	go s.writer()
	return s, nil
}

// writer 异步写入循环。
// 退出协议：收到 quit 信号后排空队列内剩余条目再退出（优雅关闭不丢已入队日志）。
// 注意：数据 channel 永不 close——close 与并发 send 之间存在固有竞态
// （send on closed channel panic，select+default 无法规避），改用 quit/done
// 信号对 + 锁内 send 保证关闭语义。
func (s *LogStore) writer() {
	defer close(s.done)
	for {
		select {
		case e := <-s.ch:
			s.insert(e)
		case <-s.quit:
			// 已停止接收新写入：排空队列后退出
			for {
				select {
				case e := <-s.ch:
					s.insert(e)
				default:
					return
				}
			}
		}
	}
}

func (s *LogStore) insert(e *CallLog) {
	attempts := e.AttemptLogsJSON
	if attempts == "" && e.AttemptLogs != nil {
		b, _ := json.Marshal(e.AttemptLogs)
		attempts = string(b)
	}
	var errMsg interface{}
	if e.ErrorMsg != nil {
		errMsg = *e.ErrorMsg
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO call_logs
		(id,timestamp,apikey,apikey_id,apikey_name,retry_count,attempt_logs,provider,requested_model,actual_model,status,error_msg,duration_ms,generation_ms,prompt_tokens,completion_tokens,total_tokens,tokens_per_second,req_url,req_method,req_headers,req_body,client_req_url,upstream_url,upstream_req_headers,upstream_req_body,resp_status,resp_headers,resp_body,client_resp_status,client_resp_headers,client_resp_body,first_byte_ms,first_content_ms,last_chunk_ms,sent_bytes,last_sent_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.Timestamp, e.APIKey, e.APIKeyID, e.APIKeyName, e.RetryCount, attempts,
		e.Provider, e.RequestedModel, e.ActualModel, e.Status, errMsg, e.DurationMs, e.GenerationMs,
		e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.TokensPerSecond,
		e.ReqURL, e.ReqMethod, e.ReqHeaders, e.ReqBody, e.ClientReqURL,
		e.UpstreamURL, e.UpstreamReqHeaders, e.UpstreamReqBody,
		e.RespStatus, e.RespHeaders, e.RespBody, e.ClientRespStatus, e.ClientRespHeaders, e.ClientRespBody,
		e.FirstByteMs, e.FirstContentMs, e.LastChunkMs, e.SentBytes, e.LastSentMs)
	if err != nil {
		// v1.0.0（S7）：写入失败计数（磁盘满/库损坏时日志静默消失，与可排查性目标冲突）
		atomic.AddInt64(&s.writeFailures, 1)
		fmt.Printf("[logsdb] 写入日志失败: %v\n", err)
	}
}

// WriteAsync 异步写入（队列满时丢弃并计数，不阻塞代理主流程）。
// closed 检查与 send 在同一临界区内完成：Close 置位 closed 后不可能再有入队，
// 彻底消除旧实现"锁外 send × 锁外 close"的 send-on-closed-channel panic 竞态。
// send 为非阻塞（队列满走 default 丢弃），持锁不会长时间阻塞。
func (s *LogStore) WriteAsync(e *CallLog) {
	if s == nil || e == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		// v1.0.0（S7）：关闭后丢弃计数（Recreate/Reopen 切换窗口内的新调用日志
		// 被静默丢弃，无量化指标可查）
		atomic.AddInt64(&s.dropped, 1)
		return
	}
	select {
	case s.ch <- e:
	default:
		atomic.AddInt64(&s.dropped, 1)
	}
	s.mu.Unlock()
}

// Close 优雅关闭：停止接收新写入，等待队列排空后关闭数据库。
func (s *LogStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	close(s.quit) // 通知 writer 排空退出
	select {
	case <-s.done:
	case <-time.After(15 * time.Second):
		fmt.Printf("[logsdb] 等待日志写入超时，强制关闭\n")
		// v1.0.0（S6）：排空队列剩余条目帮助 writer 快速退出——超时后 writer 仍在
		// 排空队列并对将关的库逐条 Exec 刷错；drain 与 writer 消费并发无害（均为
		// 消费侧），被丢弃的条目本就已不可能落库，计入丢弃计数保留可观测性。
	drain:
		for {
			select {
			case <-s.ch:
				atomic.AddInt64(&s.dropped, 1)
			default:
				break drain
			}
		}
	}
	if atomic.LoadInt64(&s.dropped) > 0 {
		fmt.Printf("[logsdb] 警告：运行期间丢弃 %d 条日志（队列满/关闭后到达）\n", atomic.LoadInt64(&s.dropped))
	}
	if n := atomic.LoadInt64(&s.writeFailures); n > 0 {
		// v1.0.0（S7）：写入失败汇总（磁盘满/库损坏期间静默丢失的条数）
		fmt.Printf("[logsdb] 警告：运行期间 %d 条日志写入失败（磁盘满或库损坏）\n", n)
	}
	return s.db.Close()
}

// Reopen 重开数据库（日志路径热更）。
func (s *LogStore) Reopen(newPath string) (*LogStore, error) {
	ns, err := Open(newPath)
	if err != nil {
		return nil, err
	}
	_ = s.Close()
	return ns, nil
}

// Path 返回当前库文件路径。
func (s *LogStore) Path() string { return s.path }

// Recreate 清空并重建日志库（v1.0.0）：关闭当前库 → 删除库文件及 -wal / -shm
// 伴生文件 → 在原路径重建空库，返回新 store（调用方需替换旧引用）。
// 背景：SQLite 的 DELETE 只把页标记为空闲复用，不会把空间归还文件系统；
// 删除文件重建可即时释放全部磁盘空间。
// 容错：文件被外部工具占用导致删除失败时，重建后降级执行 SQL 清空
// （保证清空语义成立，空间待文件解锁后下次清空释放）。
// 调用方需自行防并发重建（多次 Recreate 交错会互相删掉对方的新库）。
func (s *LogStore) Recreate() (*LogStore, error) {
	if s == nil {
		return nil, fmt.Errorf("日志存储不可用")
	}
	path := s.path
	_ = s.Close() // 排空队列并关闭连接，Windows 下需先关库才能删除文件
	removed := true
	// v1.0.0（S4）：先删伴生文件再删主文件（SQLite 官方建议顺序）——旧顺序下
	// 主文件删除成功而 -wal 被占用删除失败时，重建后 WAL recovery 会尝试重放旧页，
	// 存在把已删旧数据写回新库的理论窗口。
	for _, f := range []string{path + "-shm", path + "-wal", path} {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			removed = false
			fmt.Printf("[logsdb] 删除日志文件失败 %s: %v（降级 SQL 清空，空间待文件解锁后释放）\n", f, err)
		}
	}
	ns, err := Open(path)
	if err != nil {
		return nil, fmt.Errorf("重建日志数据库失败: %w", err)
	}
	if !removed {
		// 文件未能删除（被占用）：打开的是旧文件，降级 SQL 清空保证数据被清掉。
		// v1.0.0（S7）：清空失败时返回错误而非吞掉——旧行为会让 handleLogsClear
		// 返回 ok:true 但日志实际未清，用户被误导。
		if _, err := ns.DeleteAll(); err != nil {
			_ = ns.Close()
			return nil, fmt.Errorf("日志文件被占用且降级 SQL 清空失败: %w", err)
		}
	}
	return ns, nil
}

// ===== 查询 =====

// LogQuery 查询条件。
type LogQuery struct {
	Page      int
	PageSize  int
	Status    string
	Provider  string
	Model     string
	APIKeyID  string
	Keyword   string
	StartTime string
	EndTime   string
}

// LogPage 分页结果。
type LogPage struct {
	Total    int64      `json:"total"`
	Page     int        `json:"page"`
	PageSize int        `json:"page_size"`
	Logs     []*CallLog `json:"logs"`
}

// escapeLike 转义 SQLite LIKE 通配符（配合 ESCAPE '\' 使用），
// 使用户输入的 % 与 _ 按字面匹配而非被当作通配符。
func escapeLike(s string) string {
	r := strings.ReplaceAll(s, `\`, `\\`)
	r = strings.ReplaceAll(r, `%`, `\%`)
	r = strings.ReplaceAll(r, `_`, `\_`)
	return r
}

func (q *LogQuery) buildWhere() (string, []interface{}) {
	args := []interface{}{}
	var conds []string
	if q.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, q.Status)
	}
	if q.Provider != "" {
		conds = append(conds, "provider = ?")
		args = append(args, q.Provider)
	}
	if q.Model != "" {
		conds = append(conds, "(requested_model = ? COLLATE NOCASE OR actual_model = ? COLLATE NOCASE)")
		args = append(args, q.Model, q.Model)
	}
	if q.APIKeyID != "" {
		conds = append(conds, "apikey_id = ?")
		args = append(args, q.APIKeyID)
	}
	if q.Keyword != "" {
		kw := "%" + escapeLike(q.Keyword) + "%"
		conds = append(conds, `(apikey LIKE ? ESCAPE '\' OR apikey_name LIKE ? ESCAPE '\' OR provider LIKE ? ESCAPE '\' OR requested_model LIKE ? ESCAPE '\' OR actual_model LIKE ? ESCAPE '\' OR status LIKE ? ESCAPE '\' OR error_msg LIKE ? ESCAPE '\' OR req_body LIKE ? ESCAPE '\')`)
		args = append(args, kw, kw, kw, kw, kw, kw, kw, kw)
	}
	if q.StartTime != "" {
		conds = append(conds, "timestamp >= ?")
		args = append(args, q.StartTime)
	}
	if q.EndTime != "" {
		conds = append(conds, "timestamp <= ?")
		args = append(args, q.EndTime)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// Query 分页查询日志列表（不含请求/响应详情大字段）。
func (s *LogStore) Query(q LogQuery) (*LogPage, error) {
	if q.Page <= 0 {
		q.Page = 1
	}
	if q.Page > 100000 {
		q.Page = 100000 // v1.0.0（S3）：防 (Page-1)*PageSize 回绕与荒谬深分页
	}
	if q.PageSize <= 0 {
		q.PageSize = 100
	}
	if q.PageSize > 500 {
		q.PageSize = 500 // 防止超大 page_size 拉取大字段列表拖爆内存
	}
	where, args := q.buildWhere()
	// v1.0.0（M8）：COUNT 改用窗口函数随主查询一遍扫描——keyword 命中 8 列时
	// （含 1MB 级 req_body 前导通配 LIKE，任何索引均无法命中）旧实现同条件全表
	// 扫两遍（COUNT 一遍 + SELECT 一遍），大表上秒级卡顿且阻塞异步写入。
	offset := (q.Page - 1) * q.PageSize
	rows, err := s.db.Query(`SELECT COUNT(*) OVER() AS _total, id,timestamp,apikey,apikey_id,apikey_name,retry_count,attempt_logs,provider,requested_model,actual_model,status,error_msg,duration_ms,generation_ms,prompt_tokens,completion_tokens,total_tokens,tokens_per_second,first_byte_ms,first_content_ms,last_chunk_ms,sent_bytes,last_sent_ms
		FROM call_logs`+where+` ORDER BY timestamp DESC, rowid DESC LIMIT ? OFFSET ?`,
		append(args, q.PageSize, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &LogPage{Total: 0, Page: q.Page, PageSize: q.PageSize, Logs: []*CallLog{}}
	for rows.Next() {
		e := &CallLog{}
		var attempts sql.NullString
		var errMsg sql.NullString
		var keyName sql.NullString
		var rowTotal int64
		if err := rows.Scan(&rowTotal, &e.ID, &e.Timestamp, &e.APIKey, &e.APIKeyID, &keyName, &e.RetryCount, &attempts, &e.Provider, &e.RequestedModel, &e.ActualModel, &e.Status, &errMsg, &e.DurationMs, &e.GenerationMs, &e.PromptTokens, &e.CompletionTokens, &e.TotalTokens, &e.TokensPerSecond, &e.FirstByteMs, &e.FirstContentMs, &e.LastChunkMs, &e.SentBytes, &e.LastSentMs); err != nil {
			return nil, err
		}
		if page.Total == 0 {
			page.Total = rowTotal // 窗口总数在 LIMIT 前计算，每行相同；取首行即可
		}
		e.APIKeyName = keyName.String
		e.AttemptLogsJSON = attempts.String
		if errMsg.Valid {
			v := errMsg.String
			e.ErrorMsg = &v
		}
		page.Logs = append(page.Logs, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 窗口 total 依赖返回行：当前页无行（第一页就无数据、或 offset 超界）时
	// 单独 COUNT 兜底——total 语义是"满足条件的总行数"，与页码无关（前端分页器依赖）。
	// 该兜底仅在空页场景发生，常规页省掉整遍 COUNT 扫描。
	if len(page.Logs) == 0 {
		var total int64
		if err := s.db.QueryRow("SELECT COUNT(*) FROM call_logs"+where, args...).Scan(&total); err != nil {
			return nil, err
		}
		page.Total = total
	}
	return page, nil
}

// Get 查询单条日志详情（含请求/响应完整字段）。
func (s *LogStore) Get(id string) (*CallLog, error) {
	row := s.db.QueryRow(`SELECT id,timestamp,apikey,apikey_id,apikey_name,retry_count,attempt_logs,provider,requested_model,actual_model,status,error_msg,duration_ms,generation_ms,prompt_tokens,completion_tokens,total_tokens,tokens_per_second,req_url,req_method,req_headers,req_body,client_req_url,upstream_url,upstream_req_headers,upstream_req_body,resp_status,resp_headers,resp_body,client_resp_status,client_resp_headers,client_resp_body,first_byte_ms,first_content_ms,last_chunk_ms,sent_bytes,last_sent_ms
		FROM call_logs WHERE id = ?`, id)
	e := &CallLog{}
	var attempts, errMsg sql.NullString
	var keyName sql.NullString
	var reqURL, reqMethod, reqHeaders, reqBody, clientReqURL, upstreamURL, upstreamReqHeaders, upstreamReqBody, respHeaders, respBody, clientRespHeaders, clientRespBody sql.NullString
	var respStatus, clientRespStatus sql.NullInt64
	if err := row.Scan(&e.ID, &e.Timestamp, &e.APIKey, &e.APIKeyID, &keyName, &e.RetryCount, &attempts, &e.Provider, &e.RequestedModel, &e.ActualModel, &e.Status, &errMsg, &e.DurationMs, &e.GenerationMs, &e.PromptTokens, &e.CompletionTokens, &e.TotalTokens, &e.TokensPerSecond, &reqURL, &reqMethod, &reqHeaders, &reqBody, &clientReqURL, &upstreamURL, &upstreamReqHeaders, &upstreamReqBody, &respStatus, &respHeaders, &respBody, &clientRespStatus, &clientRespHeaders, &clientRespBody, &e.FirstByteMs, &e.FirstContentMs, &e.LastChunkMs, &e.SentBytes, &e.LastSentMs); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	e.APIKeyName = keyName.String
	e.AttemptLogsJSON = attempts.String
	if errMsg.Valid {
		v := errMsg.String
		e.ErrorMsg = &v
	}
	e.ReqURL = reqURL.String
	e.ReqMethod = reqMethod.String
	e.ReqHeaders = reqHeaders.String
	e.ReqBody = reqBody.String
	e.ClientReqURL = clientReqURL.String
	e.UpstreamURL = upstreamURL.String
	e.UpstreamReqHeaders = upstreamReqHeaders.String
	e.UpstreamReqBody = upstreamReqBody.String
	e.RespStatus = int(respStatus.Int64)
	e.RespHeaders = respHeaders.String
	e.RespBody = respBody.String
	e.ClientRespStatus = int(clientRespStatus.Int64)
	e.ClientRespHeaders = clientRespHeaders.String
	e.ClientRespBody = clientRespBody.String
	return e, nil
}

// ===== 统计聚合 =====

// TimelinePoint 时间序列点。
type TimelinePoint struct {
	Date             string  `json:"date"`
	Calls            int64   `json:"calls"`
	SuccessRate      float64 `json:"success_rate"`
	AvgDurationMs    float64 `json:"avg_duration_ms"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
}

// DimensionRow 通用维度统计行。
type DimensionRow struct {
	Key           string  `json:"-"`
	Label         string  `json:"-"`
	Calls         int64   `json:"calls"`
	Success       int64   `json:"success"`
	Failure       int64   `json:"failure"`
	Tokens        int64   `json:"tokens"`
	SuccessRate   float64 `json:"success_rate"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
}

// Summary 全局汇总。
type Summary struct {
	TotalCalls    int64   `json:"total_calls"`
	Success       int64   `json:"success"`
	Failure       int64   `json:"failure"`
	SuccessRate   float64 `json:"success_rate"`
	TotalTokens   int64   `json:"total_tokens"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
}

// DashboardStats 仪表盘统计。
type DashboardStats struct {
	TodayCalls       int64
	TotalCalls       int64
	SuccessCalls     int64
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

func successRateExpr() string {
	return `CAST(SUM(CASE WHEN status='success' THEN 1 ELSE 0 END) AS REAL) / COUNT(*)`
}

// Dashboard 查询仪表盘统计。
func (s *LogStore) Dashboard(todayStart string) (*DashboardStats, error) {
	st := &DashboardStats{}
	row := s.db.QueryRow(`SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN status='success' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(total_tokens),0)
		FROM call_logs`)
	if err := row.Scan(&st.TotalCalls, &st.SuccessCalls, &st.PromptTokens, &st.CompletionTokens, &st.TotalTokens); err != nil {
		return nil, err
	}
	err := s.db.QueryRow(`SELECT COUNT(*) FROM call_logs WHERE timestamp >= ?`, todayStart).Scan(&st.TodayCalls)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	return st, nil
}

// Timeline 时间序列（granularity: "day" 或 "hour"，prefixLen 10 或 13）。
func (s *LogStore) Timeline(start, end string, prefixLen int) ([]TimelinePoint, error) {
	fn := "substr(timestamp,1,10)"
	if prefixLen == 13 {
		fn = "substr(timestamp,1,13)"
	}
	rows, err := s.db.Query(`SELECT `+fn+`, COUNT(*), `+successRateExpr()+`,
			CAST(AVG(duration_ms) AS REAL), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(total_tokens),0)
		FROM call_logs WHERE timestamp >= ? AND timestamp <= ? GROUP BY 1 ORDER BY 1`, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TimelinePoint
	for rows.Next() {
		var p TimelinePoint
		var rate, avg sql.NullFloat64
		if err := rows.Scan(&p.Date, &p.Calls, &rate, &avg, &p.PromptTokens, &p.CompletionTokens, &p.TotalTokens); err != nil {
			return nil, err
		}
		p.SuccessRate = rate.Float64
		p.AvgDurationMs = avg.Float64
		out = append(out, p)
	}
	return out, rows.Err()
}

// GroupBy 按 provider / actual_model / apikey / apikey_id 分组统计。
func (s *LogStore) GroupBy(start, end, field string) ([]DimensionRow, error) {
	valid := map[string]string{
		"provider":  "provider",
		"model":     "actual_model",
		"apikey":    "apikey",
		"apikey_id": "apikey_id",
	}
	col, ok := valid[field]
	if !ok {
		return nil, fmt.Errorf("非法分组字段: %s", field)
	}
	extra := ""
	if field == "apikey" {
		// SQLite 裸列（非聚合列）取组内任意行，Key 改名后名称不确定；
		// 统一用 MAX(apikey_name) 与 apikey_id 分支保持一致语义
		extra = ", MAX(apikey_name)"
	} else if field == "apikey_id" {
		extra = ", MAX(apikey_name)"
	}
	rows, err := s.db.Query(`SELECT `+col+extra+`, COUNT(*),
			COALESCE(SUM(CASE WHEN status='success' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN status!='success' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(total_tokens),0), `+successRateExpr()+`, CAST(AVG(duration_ms) AS REAL)
		FROM call_logs WHERE timestamp >= ? AND timestamp <= ? GROUP BY `+col+` ORDER BY COUNT(*) DESC`, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DimensionRow
	for rows.Next() {
		var r DimensionRow
		var key string
		var rate, avg sql.NullFloat64
		var label sql.NullString
		var dest []interface{}
		if field == "apikey" || field == "apikey_id" {
			dest = []interface{}{&key, &label, &r.Calls, &r.Success, &r.Failure, &r.Tokens, &rate, &avg}
		} else {
			dest = []interface{}{&key, &r.Calls, &r.Success, &r.Failure, &r.Tokens, &rate, &avg}
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		r.Key = key
		r.Label = label.String
		r.SuccessRate = rate.Float64
		r.AvgDurationMs = avg.Float64
		out = append(out, r)
	}
	return out, rows.Err()
}

// Summary 汇总统计。
func (s *LogStore) Summary(start, end string) (*Summary, error) {
	row := s.db.QueryRow(`SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN status='success' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN status!='success' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(total_tokens),0), CAST(AVG(duration_ms) AS REAL)
		FROM call_logs WHERE timestamp >= ? AND timestamp <= ?`, start, end)
	sm := &Summary{}
	var avg sql.NullFloat64
	if err := row.Scan(&sm.TotalCalls, &sm.Success, &sm.Failure, &sm.TotalTokens, &avg); err != nil {
		return nil, err
	}
	if sm.TotalCalls > 0 {
		sm.SuccessRate = float64(sm.Success) / float64(sm.TotalCalls)
	}
	sm.AvgDurationMs = avg.Float64
	return sm, nil
}

// NowRFC3339 当前本地时间（ISO 8601）。
func NowRFC3339() string { return time.Now().Format(time.RFC3339) }

// ===== 删除 =====

// Delete 删除单条日志，返回删除行数。
func (s *LogStore) Delete(id string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM call_logs WHERE id = ?`, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteAll 清空全部日志，返回删除行数。
func (s *LogStore) DeleteAll() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM call_logs`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Count 返回当前日志总行数（供清空前统计删除条数）。
func (s *LogStore) Count() (int64, error) {
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM call_logs`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
