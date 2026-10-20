package api

// 回归测试：
//   日志查询/详情/单删/仪表盘/统计 store 引用锁内获取 + closed 重试
//   设置页日志路径切换持 logOpMu 写锁（正向切换与热加载回滚路径）
//   fetch-models ctx 贯穿（NewRequestWithContext）+ 表单模式 10 分钟总预算
//
// 竞态窗口为纳秒级，短时并发压测难以稳定复现；
// 此处以固定次数并发压测断言 0 次 500。
// 10 分钟总预算为时间类行为，单测无法压缩等待——ctx 链路（取消传导、预算
// 耗尽停止尝试的判定入口）由本文件两个 ctx 取消用例覆盖，预算值以代码评审保证。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llmproxy/internal/logsdb"
)

// ===== 公用：复刻 main 层 logStoreSwitcher 的测试切换器 =====

// logPathSwitcher 复刻 main.go logStoreSwitcher 的锁语义（简化版，无自愈分支）：
// recreate 对应清空重建（OnLogRecreate）、changePath 对应路径热切换（OnLogPathChange），
// 均在切换完成后经回调同步替换 Server 内的 store 引用（与 main 的 onSwap 一致）。
type logPathSwitcher struct {
	mu    sync.Mutex
	store *logsdb.LogStore
	swap  func(*logsdb.LogStore)
}

func (s *logPathSwitcher) recreate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ns, err := s.store.Recreate()
	if err != nil {
		return err
	}
	s.store = ns
	s.swap(ns)
	return nil
}

func (s *logPathSwitcher) changePath(p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ns, err := s.store.Reopen(p)
	if err != nil {
		return err
	}
	s.store = ns
	s.swap(ns)
	return nil
}

// openLogPathSwitcher 打开库并装配切换器（写入 seed 条日志并等待落库）。
// 清理时关闭切换器**当前**活跃库——路径切换后初始 store 已被 Reopen 内部
// 关闭，若只关初始引用会让最终库的 SQLite 句柄泄漏到 TempDir 清理
// （Windows 下 RemoveAll 报 file in use）。
func openLogPathSwitcher(t *testing.T, s *Server, dbPath string, seed int) *logPathSwitcher {
	t.Helper()
	store, err := logsdb.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	s.SetLogStore(store)
	sw := &logPathSwitcher{store: store, swap: func(ns *logsdb.LogStore) { s.SetLogStore(ns) }}
	t.Cleanup(func() {
		sw.mu.Lock()
		cur := sw.store
		sw.mu.Unlock()
		_ = cur.Close()
	})
	s.OnLogRecreate = sw.recreate
	s.OnLogPathChange = sw.changePath
	for i := 0; i < seed; i++ {
		writeLogEntry(t, store, fmt.Sprintf("id-%d", i))
	}
	waitForCount(t, store, int64(seed))
	return sw
}

// ===== 并发清空重建 vs 并发查询 =====

// TestLogsQueryConcurrentWithClear 并发清空（重建）+ 并发查询期间 0 次 500。
// 关键前提：查询的 store 引用必须在读锁内获取；若在锁外获取，阻塞在 RLock 的
// 查询线程醒来后拿到的仍是已关旧库。清空持写锁与查询互斥，等待的查询醒来即拿新 store。
// 清空为真实磁盘重建（删库+建库+fsync），固定 10 次而非时间窗循环——避免高频
// 重建压垮磁盘 I/O；竞态窗口由「重建执行期间持续并发查询」保证。
func TestLogsQueryConcurrentWithClear(t *testing.T) {
	s, dir := newTestServer(t)
	openLogPathSwitcher(t, s, filepath.Join(dir, "g1.db"), 5)

	var hits, queries int64
	stop := make(chan struct{})
	var stopOnce sync.Once
	shutdown := func() { stopOnce.Do(func() { close(stop) }) }
	// 兜底：清空 goroutine 异常卡住时不让测试挂死
	time.AfterFunc(30*time.Second, shutdown)
	defer shutdown()

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				req := httptest.NewRequest("GET", "/api/logs?page=1&page_size=10", nil)
				rec := httptest.NewRecorder()
				s.handleLogsList(rec, req)
				atomic.AddInt64(&queries, 1)
				if rec.Code == http.StatusInternalServerError {
					atomic.AddInt64(&hits, 1)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			req := httptest.NewRequest("DELETE", "/api/logs", nil)
			rec := httptest.NewRecorder()
			s.handleLogsClear(rec, req)
		}
		shutdown()
	}()
	wg.Wait()
	if hits > 0 {
		t.Fatalf("并发清空+查询期间出现 %d 次 500（共 %d 次查询）——store 引用应在读锁内获取前实测 15 秒可捕获 3943 次", hits, atomic.LoadInt64(&queries))
	}
}

// ===== 并发路径切换（经 handleSettingsPut）vs 并发查询 =====

// TestLogsQueryConcurrentWithPathChange 并发路径热切换 + 并发查询期间 0 次 500。
// 关键前提：切换的「关旧库→开新库→swap」必须在 handleSettingsPut 内持 logOpMu
// 写锁完成，否则与查询零互斥；查询引用在读锁内获取，全程互斥。
// 切换为真实磁盘操作（关库+开新库），固定 10 次交替而非时间窗循环（理由同上）。
func TestLogsQueryConcurrentWithPathChange(t *testing.T) {
	s, dir := newTestServer(t)
	dbA := filepath.Join(dir, "g2a.db")
	dbB := filepath.Join(dir, "g2b.db")
	openLogPathSwitcher(t, s, dbA, 5)

	var hits, queries, switches int64
	stop := make(chan struct{})
	var stopOnce sync.Once
	shutdown := func() { stopOnce.Do(func() { close(stop) }) }
	time.AfterFunc(30*time.Second, shutdown)
	defer shutdown()

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				req := httptest.NewRequest("GET", "/api/logs?page=1&page_size=10", nil)
				rec := httptest.NewRecorder()
				s.handleLogsList(rec, req)
				atomic.AddInt64(&queries, 1)
				if rec.Code == http.StatusInternalServerError {
					atomic.AddInt64(&hits, 1)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			p := dbA
			if i%2 == 1 {
				p = dbB
			}
			// Windows 路径含反斜杠，须 json.Marshal 转义后嵌入请求体
			//（手工拼接会产生非法 JSON 转义序列，PUT 全部 400）
			body, _ := json.Marshal(map[string]interface{}{"log": map[string]string{"db_path": p}})
			req := httptest.NewRequest("PUT", "/api/settings", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			s.handleSettingsPut(rec, req)
			if rec.Code == http.StatusOK {
				atomic.AddInt64(&switches, 1)
			}
		}
		shutdown()
	}()
	wg.Wait()
	if atomic.LoadInt64(&switches) == 0 {
		t.Fatal("压测期间未发生任何一次成功的路径切换，用例未覆盖目标竞态")
	}
	if hits > 0 {
		t.Fatalf("并发路径切换+查询期间出现 %d 次 500（共 %d 次查询 / %d 次切换）——切换应持 logOpMu 写锁前实测 15 秒可捕获 2171 次",
			hits, atomic.LoadInt64(&queries), atomic.LoadInt64(&switches))
	}
}

// TestLogsQueryConcurrentWithHotReloadSwitch 配置热加载路径切换的补充用例。
// （SwitchLogPathLocked）与并发查询同样 0 次 500。 main 的 Watch goroutine
// 直接调用 OnLogPathChange 回调绕过 logOpMu——背靠背切换仍会捕获
// 176 次 500（closed 重试一次在高频切换窗口内不足）；统一持锁后消除。
func TestLogsQueryConcurrentWithHotReloadSwitch(t *testing.T) {
	s, dir := newTestServer(t)
	dbA := filepath.Join(dir, "hr-a.db")
	dbB := filepath.Join(dir, "hr-b.db")
	openLogPathSwitcher(t, s, dbA, 5)

	var hits, queries, switches int64
	stop := make(chan struct{})
	var stopOnce sync.Once
	shutdown := func() { stopOnce.Do(func() { close(stop) }) }
	time.AfterFunc(30*time.Second, shutdown)
	defer shutdown()

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				req := httptest.NewRequest("GET", "/api/logs?page=1&page_size=10", nil)
				rec := httptest.NewRecorder()
				s.handleLogsList(rec, req)
				atomic.AddInt64(&queries, 1)
				if rec.Code == http.StatusInternalServerError {
					atomic.AddInt64(&hits, 1)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			p := dbA
			if i%2 == 1 {
				p = dbB
			}
			if err := s.SwitchLogPathLocked(p); err == nil {
				atomic.AddInt64(&switches, 1)
			}
		}
		shutdown()
	}()
	wg.Wait()
	if atomic.LoadInt64(&switches) == 0 {
		t.Fatal("压测期间未发生任何一次成功的热加载路径切换，用例未覆盖目标竞态")
	}
	if hits > 0 {
		t.Fatalf("热加载路径并发切换+查询期间出现 %d 次 500（共 %d 次查询 / %d 次切换）——热加载应经 SwitchLogPathLocked 持锁",
			hits, atomic.LoadInt64(&queries), atomic.LoadInt64(&switches))
	}
}

// ===== fetch-models ctx 贯穿与总预算 =====

// TestFetchUpstreamModelsContextCancel fetchUpstreamModels 接收 ctx——
// ctx 取消后立即返回，不再依赖 per-request 超时兜底。
// 若 http.NewRequest 不带 ctx，取消的请求仍会挂满 60s。
func TestFetchUpstreamModelsContextCancel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Second) // 挂住的上游：ctx 未传导时会阻塞至 60s per-request 超时
	}))
	defer up.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := fetchUpstreamModels(ctx, "openai", up.URL, nil, "sk-x", "", 60*time.Second)
	if err == nil {
		t.Fatal("ctx 已取消应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("ctx 取消应立即返回，实际耗时 %v（NewRequestWithContext 未传导 ctx）", elapsed)
	}
}

// TestFetchModelsFormContextCancel 表单模式整体预算经 r.Context() 派生——
// 请求 ctx 已取消时首条 Key 前即停止尝试并注明总超时，立即 502。
// 若无总预算，串行 50 Key × 60s 可挂起约 50 分钟，且客户端断开无法终止。
func TestFetchModelsFormContextCancel(t *testing.T) {
	s, _ := newTestServer(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Second)
	}))
	defer up.Close()
	body := `{"base_url":"` + up.URL + `","api_type":"openai",` +
		`"api_keys":[{"id":"k1","key":"sk-a"},{"id":"k2","key":"sk-b"}]}`
	req := httptest.NewRequest("POST", "/api/providers/fetch-models", strings.NewReader(body))
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	start := time.Now()
	s.handleFetchModelsForm(rec, req)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("请求 ctx 已取消应立即返回，实际耗时 %v", elapsed)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("应 502，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "总超时") {
		t.Fatalf("错误消息应注明总超时停止，实际 %s", rec.Body.String())
	}
}
