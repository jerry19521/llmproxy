// LLMProxy 大模型代理工具。
// 统一管理各供应商的大模型服务：代理转发、身份认证、路由与熔断、调用日志与管理界面。
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"llmproxy/internal/api"
	"llmproxy/internal/config"
	"llmproxy/internal/core"
	"llmproxy/internal/logsdb"
	"llmproxy/internal/proxy"
)

//go:embed web
var webFS embed.FS

var buildVersion = "1.8.6"

func main() {
	configPath := flag.String("config", "config.yml", "配置文件路径")
	flagPort := flag.Int("port", -1, "覆盖监听端口")
	flagHost := flag.String("host", "", "覆盖监听地址")
	flag.Parse()

	mgr := config.NewManager(*configPath)
	cfg, err := mgr.Load()
	if err != nil {
		fmt.Printf("加载配置失败: %v\n", err)
		os.Exit(1)
	}
	// 启动参数覆盖：仅影响运行值，不写回 config.yml（避免临时参数经后续保存被意外持久化）
	if *flagPort > 0 || *flagHost != "" {
		host, port := cfg.Listen.Host, cfg.Listen.Port
		if *flagHost != "" {
			host = *flagHost
		}
		if *flagPort > 0 {
			port = *flagPort
		}
		mgr.SetListenOverride(host, port)
	}

	state := core.NewState(mgr)

	// 打开日志数据库（无论日志开关是否开启都需打开：已有日志仍可查询/删除；开关仅控制写入）
	logStore, err := logsdb.Open(cfg.Log.DBPath)
	if err != nil {
		fmt.Printf("打开日志数据库失败: %v\n", err)
		os.Exit(1)
	}
	if !cfg.Log.EnabledVal() {
		fmt.Printf("[logs] 日志记录已关闭（log.enabled=false），新调用不写入日志库\n")
	}

	proxyHandler := proxy.NewHandler(state)
	proxyHandler.SetLogStore(logStore)

	listenCfg := mgr.EffectiveListen()
	apiServer := &api.Server{Mgr: mgr, State: state, Listen: fmt.Sprintf("%s:%d", listenCfg.Host, listenCfg.Port)}
	apiServer.SetLogStore(logStore)

	// 日志库切换器（v1.8.2，B1 修复）：logStore 变量被三条路径并发读写——
	// 设置页改日志路径（OnLogPathChange，API 请求 goroutine）、清空日志重建
	//（OnLogRecreate，API 请求 goroutine）、配置热加载路径切换（Watch 后台
	// goroutine）。本切换器用独立互斥将三条路径对 logStore 的读-改-写串行化，
	// 消除数据竞态与 store 引用交错；api 层 logOpMu 另将三条路径与日志查询
	// 互斥（v1.8.5 G1/G2：查询读锁内取引用，设置页 / 清空 / 热加载均持写锁）。
	logSw := newLogStoreSwitcher(logStore, func(ns *logsdb.LogStore) {
		proxyHandler.SetLogStore(ns)
		apiServer.SetLogStore(ns)
	})
	apiServer.OnLogPathChange = logSw.changePath
	apiServer.OnLogRecreate = logSw.recreate

	// 配置热加载
	mgr.SetOnChange(func(c *config.Config) {
		oldLogPath := state.Snap().LogDBPath
		state.Reload()
		fmt.Printf("[hot-reload] 配置已重新加载\n")
		if state.Snap().LogDBPath != oldLogPath {
			// v1.8.5（G2）补全：热加载路径切换改经 api 层持锁入口——旧实现直接调
			// logSw.changePath 绕过 logOpMu，切换与日志查询零互斥（查询持旧引用报
			// database is closed 瞬态 500）；与设置页 / 清空两条路径统一在同一把锁下。
			_ = apiServer.SwitchLogPathLocked(state.Snap().LogDBPath)
		}
	})
	mgr.Watch()

	// 组装路由
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": buildVersion})
	})
	mux.Handle("/v1/", http.HandlerFunc(proxyHandler.ServeV1))
	mux.Handle("/api/", apiServer.Handler())
	mux.Handle("/", spaHandler())

	listenCfg = mgr.EffectiveListen()
	addr := net.JoinHostPort(listenCfg.Host, fmt.Sprint(listenCfg.Port))
	srv := &http.Server{
		Addr:              addr,
		Handler:           securityHeaders{mux},
		ReadHeaderTimeout: 30 * time.Second,
	}

	// 启动
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Printf("监听 %s 失败: %v\n", addr, err)
		os.Exit(1)
	}
	fmt.Printf("LLMProxy v%s 已启动: http://%s\n", buildVersion, addr)
	fmt.Printf("管理界面: http://%s/   代理端点: http://%s/v1/chat/completions | /v1/responses | /v1/messages | /v1/models\n", addr, addr)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// 优雅关闭
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		fmt.Printf("服务异常退出: %v\n", err)
	case sig := <-sigCh:
		fmt.Printf("\n收到信号 %v，正在优雅关闭...\n", sig)
	}

	// 停止接收新请求，等待在途请求（含流式）完成
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)

	// 停止后台任务
	state.Stop()

	// 确保日志落盘
	_ = logSw.close()
	mgr.Stop()
	fmt.Printf("已退出。\n")
}

// securityHeaders 为所有响应附加基础安全响应头。
type securityHeaders struct{ next http.Handler }

func (h securityHeaders) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	h.next.ServeHTTP(w, r)
}

// ===== 前端静态资源（SPA fallback）====

func spaHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		// SPA 前端路由 fallback：非静态文件路径返回 index.html
		if !strings.Contains(p, ".") {
			data, err := webFS.ReadFile("web/index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(data)
			return
		}
		http.NotFound(w, r)
	})
}

// ===== 日志库切换器（v1.8.2，B1 修复）=====

// logStoreSwitcher 管理运行态日志库的切换互斥：logStore 变量被三条路径并发读写——
// 设置页改日志路径（OnLogPathChange，API 请求 goroutine）、清空日志重建
// （OnLogRecreate，API 请求 goroutine）、配置热加载路径切换（Watch 后台 goroutine）。
// api 层 logOpMu 仅覆盖清空路径，本类型用独立互斥将三条路径对 store 的读-改-写
// 全部串行化，消除数据竞态与 store 引用交错。onSwap 在持锁期间同步执行，
// 保证 proxyHandler / apiServer 看到的 LogStore 与本类型持有的 store 始终一致。
type logStoreSwitcher struct {
	mu     sync.Mutex
	store  *logsdb.LogStore
	onSwap func(*logsdb.LogStore) // 切换后同步到 proxyHandler / apiServer（main 注入）
}

// newLogStoreSwitcher 创建切换器；onSwap 在每次成功切换后被调用（持锁期间）。
func newLogStoreSwitcher(store *logsdb.LogStore, onSwap func(*logsdb.LogStore)) *logStoreSwitcher {
	return &logStoreSwitcher{store: store, onSwap: onSwap}
}

// changePath 日志路径热更（设置页保存 / 配置热加载共用）：在新路径打开库并替换引用。
func (s *logStoreSwitcher) changePath(newPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ns, err := s.store.Reopen(newPath)
	if err != nil {
		return err
	}
	s.store = ns
	if s.onSwap != nil {
		s.onSwap(ns)
	}
	fmt.Printf("[hot-reload] 日志数据库已切换到 %s\n", newPath)
	return nil
}

// recreate 清空日志：删除库文件并原路径重建空库，磁盘空间即时释放（v1.8.1）；
// v1.8.3：与路径热更共用同一互斥——清空与路径切换不再可能交错重建/删除对方的库。
// v1.8.3（M1）：重建失败时自愈兜底——logsdb.Recreate 内部先关旧库再重建，若 Open
// 失败（磁盘满、路径被占用等）返回错误，旧 store 已关闭：proxy 写日志静默丢弃、
// 管理端查询全部 500，且无自动恢复路径。此处在返回错误前尝试原路径重新打开空库
// 恢复日志子系统；自愈也失败才真正返回错误（需重启）。
func (s *logStoreSwitcher) recreate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldPath := s.store.Path()
	ns, err := s.store.Recreate()
	if err != nil {
		fmt.Printf("[logs] 重建日志数据库失败: %v（旧库已关闭，正在尝试原路径恢复…）\n", err)
		if rs := reopenAfterFailedRecreate(oldPath); rs != nil {
			s.store = rs
			if s.onSwap != nil {
				s.onSwap(rs)
			}
			fmt.Printf("[logs] 日志数据库已自愈恢复（原路径重建空库）: %s\n", oldPath)
			return nil
		}
		fmt.Printf("[logs] 日志数据库无法恢复，日志写入与查询将不可用，请重启服务或检查磁盘后重试\n")
		return err
	}
	s.store = ns
	if s.onSwap != nil {
		s.onSwap(ns)
	}
	fmt.Printf("[logs] 日志数据库已删除并重建: %s（磁盘空间已释放）\n", oldPath)
	return nil
}

// reopenAfterFailedRecreate 重建失败后的自愈：原路径重新打开空库。
// 失败返回 nil（调用方将错误上抛，由用户重启或排查后重试）。
func reopenAfterFailedRecreate(path string) *logsdb.LogStore {
	rs, err := logsdb.Open(path)
	if err != nil {
		fmt.Printf("[logs] 自愈失败: %v\n", err)
		return nil
	}
	return rs
}

// close 关闭当前库（优雅关闭时落盘）。持锁关闭，避免与切换路径交错。
func (s *logStoreSwitcher) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.Close()
}
