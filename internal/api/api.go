// Package api 管理后台 HTTP 接口：登录认证、供应商管理、代理 Key 管理、
// 基础设置、冷却池、调用日志、仪表盘与统计。
package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"llmproxy/internal/auth"
	"llmproxy/internal/config"
	"llmproxy/internal/core"
	"llmproxy/internal/logsdb"
)

// Server 管理接口服务。
type Server struct {
	Mgr    *config.Manager
	State  *core.State
	Logs   atomic.Value // *logsdb.LogStore
	Listen string       // 运行中的监听地址（启动时确定）

	// OnLogPathChange 日志路径热更回调（由 main 注入）
	OnLogPathChange func(newPath string) error

	// OnLogRecreate 清空日志回调（，由 main 注入）：删除库文件并重建空库，
	// 成功后调用方需用新 store 替换旧引用；无回调（测试环境）时清空降级为 SQL 删除。
	OnLogRecreate func() error

	// logOpMu 日志库重建互斥：防止并发清空（双击/多管理员）交错重建互相删掉新库。
	// 改为读写锁——清空（重建）持写锁，日志查询/单删持读锁：
	// 重建窗口（关旧库→删文件→重开→swap 引用）内并发查询不再拿到已关闭的旧
	// store 报 database is closed 瞬态 500，而是阻塞等待重建完成后用新 store 正常返回。
	logOpMu sync.RWMutex
}

// SetLogStore 替换日志存储。
func (s *Server) SetLogStore(l *logsdb.LogStore) { s.Logs.Store(l) }

// LogStore 获取当前日志存储。
func (s *Server) LogStore() *logsdb.LogStore {
	if v, ok := s.Logs.Load().(*logsdb.LogStore); ok {
		return v
	}
	return nil
}

// SwitchLogPathLocked 以 logOpMu 写锁执行日志路径切换。
// 配置热加载 Watch goroutine 直接调用 OnLogPathChange 回调（绕过锁），
// 切换的「关旧库→开新库→swap」与并发查询零互斥——G1 读接口的 closed 重试一次
// 兜底在高频切换窗口内不足（实测背靠背切换仍捕获 500）；热加载路径统一经本
// 方法进入后，三条切换路径（设置页 handleSettingsPut / 清空 handleLogsClear /
// 热加载）全部持同一把锁，查询读锁全程互斥。锁序 logOpMu → main 层 logSw.mu
// 与另两条路径一致，无死锁。
func (s *Server) SwitchLogPathLocked(newPath string) error {
	s.logOpMu.Lock()
	defer s.logOpMu.Unlock()
	if s.OnLogPathChange == nil {
		return nil
	}
	return s.OnLogPathChange(newPath)
}

// writeJSON 输出 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr 输出统一错误 JSON。
func writeErr(w http.ResponseWriter, status int, message, code string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]string{"message": message, "code": code},
	})
}

// writeAuthErr 输出认证类错误 JSON，error 含 message、type、code 三个字段，
// 与登录失败响应格式一致。
func writeAuthErr(w http.ResponseWriter, status int, message, errType, code string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]string{"message": message, "type": errType, "code": code},
	})
}

// ===== JWT 认证中间件 =====

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Authorization")
		if !strings.HasPrefix(hdr, "Bearer ") {
			s.logAuthFailure(r, "缺少 Bearer 凭据")
			writeAuthErr(w, http.StatusUnauthorized, "unauthorized", "authentication_error", "unauthorized")
			return
		}
		token := strings.TrimPrefix(hdr, "Bearer ")
		cfg := s.Mgr.Cfg()
		username, err := auth.VerifyToken(token, cfg.JWT.Secret, cfg.Admin.Password)
		if err != nil {
			s.logAuthFailure(r, err.Error())
			writeAuthErr(w, http.StatusUnauthorized, "unauthorized", "authentication_error", "unauthorized")
			return
		}
		// 校验 token 签发用户与当前管理员一致——修改 admin.username
		// 后旧 token（sub 为旧名）立即失效，而非持续有效到自然过期。
		if username != cfg.Admin.Username {
			s.logAuthFailure(r, "token 签发用户与当前管理员不一致")
			writeAuthErr(w, http.StatusUnauthorized, "unauthorized", "authentication_error", "unauthorized")
			return
		}
		next(w, r)
	}
}

// logAuthFailure 记录管理接口认证失败（来源 IP + 原因）：暴力尝试 token 的
// 行为否则不留任何痕迹。仅打印失败原因，不打印 token 本身。
func (s *Server) logAuthFailure(r *http.Request, reason string) {
	cfg := s.Mgr.Cfg()
	fmt.Printf("[auth] 管理接口认证失败 ip=%s 原因=%s\n", auth.ExtractClientIP(r, cfg.IPTrustLevel), reason)
}

// Handler 返回管理接口路由（含 JWT 保护）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 登录（无需认证）
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	// 认证保护区域
	mux.HandleFunc("PUT /api/password", s.requireAuth(s.handlePassword))

	mux.HandleFunc("GET /api/providers", s.requireAuth(s.handleProviderList))
	mux.HandleFunc("POST /api/providers", s.requireAuth(s.handleProviderCreate))
	mux.HandleFunc("GET /api/providers/fetch-models", s.requireAuth(s.handleFetchModelsByID))
	mux.HandleFunc("POST /api/providers/fetch-models", s.requireAuth(s.handleFetchModelsForm))
	mux.HandleFunc("GET /api/providers/{id}", s.requireAuth(s.handleProviderGet))
	mux.HandleFunc("PUT /api/providers/{id}", s.requireAuth(s.handleProviderUpdate))
	mux.HandleFunc("DELETE /api/providers/{id}", s.requireAuth(s.handleProviderDelete))
	mux.HandleFunc("POST /api/providers/{id}/enable", s.requireAuth(s.handleProviderEnable(true)))
	mux.HandleFunc("POST /api/providers/{id}/disable", s.requireAuth(s.handleProviderEnable(false)))
	mux.HandleFunc("POST /api/providers/test", s.requireAuth(s.handleProviderTest))

	mux.HandleFunc("GET /api/apikeys", s.requireAuth(s.handleAPIKeyList))
	mux.HandleFunc("POST /api/apikeys", s.requireAuth(s.handleAPIKeyCreate))
	mux.HandleFunc("GET /api/apikeys/{id}", s.requireAuth(s.handleAPIKeyGet))
	mux.HandleFunc("PUT /api/apikeys/{id}", s.requireAuth(s.handleAPIKeyUpdate))
	mux.HandleFunc("DELETE /api/apikeys/{id}", s.requireAuth(s.handleAPIKeyDelete))
	mux.HandleFunc("POST /api/apikeys/{id}/enable", s.requireAuth(s.handleAPIKeyEnable(true)))
	mux.HandleFunc("POST /api/apikeys/{id}/disable", s.requireAuth(s.handleAPIKeyEnable(false)))

	mux.HandleFunc("GET /api/settings", s.requireAuth(s.handleSettingsGet))
	mux.HandleFunc("PUT /api/settings", s.requireAuth(s.handleSettingsPut))

	mux.HandleFunc("GET /api/cooldowns", s.requireAuth(s.handleCooldownList))
	mux.HandleFunc("POST /api/cooldowns/remove", s.requireAuth(s.handleCooldownRemove))
	mux.HandleFunc("POST /api/cooldowns/clear", s.requireAuth(s.handleCooldownClear))

	mux.HandleFunc("GET /api/logs", s.requireAuth(s.handleLogsList))
	mux.HandleFunc("DELETE /api/logs", s.requireAuth(s.handleLogsClear))
	mux.HandleFunc("GET /api/logs/{id}", s.requireAuth(s.handleLogDetail))
	mux.HandleFunc("DELETE /api/logs/{id}", s.requireAuth(s.handleLogDelete))

	mux.HandleFunc("GET /api/models", s.requireAuth(s.handleModelsList))
	mux.HandleFunc("GET /api/dashboard", s.requireAuth(s.handleDashboard))
	mux.HandleFunc("GET /api/statistics", s.requireAuth(s.handleStatistics))

	// 管理接口不允许跨域：对预检返回 405；请求体统一限制 8MB（落地，
	// 防恶意/误操作超大 body 耗尽内存；代理转发端点在 proxy 层另有 64MB 限制）
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
		}
		mux.ServeHTTP(w, r)
	})
}

// ===== 登录 / 密码 =====

// loginThrottle 登录失败限流状态：按来源 IP 的滑动窗口计数。
// 旧实现登录接口对任意来源的失败尝试不做计数/退避/锁定，配合默认监听 0.0.0.0，
// 任何能路由到该端口的主机都可长期离线爆破管理员密码（bcrypt 拖慢单次校验，
// 但可无限持续）。IP 维度限流避免全局锁被恶意利用锁死管理员。
var loginThrottle = struct {
	sync.Mutex
	fails map[string][]time.Time
}{fails: map[string][]time.Time{}}

const (
	loginFailWindow   = 15 * time.Minute // 失败记忆窗口
	loginFailMaxCount = 5                // 窗口内达到该次数后拒绝新尝试，直至滑出窗口

	maxIPTrustLevel = 16 // ip_trust_level 取值上限（XFF 层级实际不超过个位数，此处仅防御异常输入）
)

// loginRemoteHost 提取来源 IP（RemoteAddr；XFF 在 ip_trust_level≥1 时不受信，不用于限流）。
func loginRemoteHost(remote string) string {
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return host
	}
	return remote
}

// loginBlockedByIP 返回该 IP 是否因窗口内失败次数达到上限而被暂时拒绝。
func loginBlockedByIP(remote string) (bool, time.Duration) {
	host := loginRemoteHost(remote)
	loginThrottle.Lock()
	defer loginThrottle.Unlock()
	now := time.Now()
	list := loginThrottle.fails[host]
	keep := list[:0]
	oldest := time.Time{}
	for _, t := range list {
		if now.Sub(t) < loginFailWindow {
			keep = append(keep, t)
			if oldest.IsZero() || t.Before(oldest) {
				oldest = t
			}
		}
	}
	loginThrottle.fails[host] = keep
	if len(keep) >= loginFailMaxCount {
		return true, loginFailWindow - now.Sub(oldest)
	}
	return false, 0
}

func loginRecordFailure(remote string) {
	host := loginRemoteHost(remote)
	loginThrottle.Lock()
	defer loginThrottle.Unlock()
	loginThrottle.fails[host] = append(loginThrottle.fails[host], time.Now())
}

func loginRecordSuccess(remote string) {
	host := loginRemoteHost(remote)
	loginThrottle.Lock()
	defer loginThrottle.Unlock()
	delete(loginThrottle.fails, host)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body", "invalid_request")
		return
	}
	// 失败次数达到上限时拒绝尝试（429），窗口滑动自动解锁；成功登录清零
	if blocked, wait := loginBlockedByIP(r.RemoteAddr); blocked {
		writeErr(w, http.StatusTooManyRequests,
			fmt.Sprintf("登录失败次数过多，请约 %d 秒后重试", int(wait.Seconds())+1), "rate_limited")
		return
	}
	cfg := s.Mgr.Cfg()
	if !auth.ConstantTimeEquals(body.Username, cfg.Admin.Username) || !auth.CheckPassword(cfg.Admin.Password, body.Password) {
		loginRecordFailure(r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"error": map[string]string{"message": "invalid credentials", "type": "authentication_error", "code": "invalid_credentials"},
		})
		return
	}
	loginRecordSuccess(r.RemoteAddr)
	token, expiresAt := auth.IssueToken(cfg.JWT.Secret, cfg.Admin.Username, cfg.Admin.Password, cfg.JWT.ExpireHours)
	writeJSON(w, http.StatusOK, map[string]string{
		"token":      token,
		"expires_at": expiresAt.Format(time.RFC3339),
	})
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body", "invalid_request")
		return
	}
	if body.NewPassword == "" {
		writeErr(w, http.StatusBadRequest, "new password cannot be empty", "invalid_password")
		return
	}
	cfg := s.Mgr.Cfg()
	if !auth.CheckPassword(cfg.Admin.Password, body.OldPassword) {
		writeErr(w, http.StatusBadRequest, "old password incorrect", "invalid_old_password")
		return
	}
	hash, err := auth.HashPassword(body.NewPassword)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "hash password failed", "internal_error")
		return
	}
	if err := s.Mgr.Update(func(c *config.Config) { c.Admin.Password = hash }); err != nil {
		writeErr(w, http.StatusInternalServerError, "save config failed: "+err.Error(), "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ===== 设置 =====

type settingsPayload struct {
	Listen *struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	} `json:"listen"`
	IPTrustLevel *int `json:"ip_trust_level"`
	Routing      *struct {
		Strategy string `json:"strategy"`
	} `json:"routing"`
	Cooldown *struct {
		Enabled         *bool `json:"enabled"`
		DurationSeconds int   `json:"duration_seconds"`
		RetryCount      int   `json:"retry_count"`
	} `json:"cooldown"`
	UpstreamTimeoutSeconds *int    `json:"upstream_timeout_seconds"`
	TestMessage            *string `json:"test_message"`
	DefaultUserAgent       *string `json:"default_user_agent"` // 发往上游的默认 UA；空=不设置（Go 默认）；供应商自定义头优先
	Log                    *struct {
		Enabled *bool  `json:"enabled"` // 是否记录调用日志（）
		DBPath  string `json:"db_path"`
	} `json:"log"`
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	snap := s.State.Snap()
	listen := s.Mgr.EffectiveListen() // 显示实际运行值（含启动参数覆盖），与配置文件值区分
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"listen":         map[string]interface{}{"host": listen.Host, "port": listen.Port},
		"ip_trust_level": snap.IPTrustLevel,
		"routing":        map[string]string{"strategy": snap.Strategy},
		"cooldown": map[string]interface{}{
			"enabled":          snap.CooldownEnabled,
			"duration_seconds": snap.CooldownSeconds,
			"retry_count":      snap.CooldownRetryCount,
		},
		"upstream_timeout_seconds": snap.UpstreamTimeoutSeconds,
		"test_message":             snap.TestMessage,
		"default_user_agent":       snap.DefaultUserAgent,
		"log":                      map[string]interface{}{"enabled": snap.LogEnabled, "db_path": snap.LogDBPath},
		"running_listen":           s.Listen,
	})
}

func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	var body settingsPayload
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body", "invalid_request")
		return
	}
	restartRequired := false
	// listen 变更：写入 yml，重启生效。比较基准为实际运行值（含启动参数覆盖，L8）：
	// 覆盖存在时界面显示的是覆盖值，用户未修改则不会误把覆盖值写入配置文件；
	// 用户显式修改则新值正常落盘（下次不带参数启动时生效）。
	if body.Listen != nil {
		host := strings.TrimSpace(body.Listen.Host)
		if host == "" || body.Listen.Port <= 0 || body.Listen.Port > 65535 {
			writeErr(w, http.StatusBadRequest, "invalid listen host/port", "invalid_request")
			return
		}
		cur := s.Mgr.EffectiveListen()
		if host != cur.Host || body.Listen.Port != cur.Port {
			if err := s.Mgr.Update(func(c *config.Config) { c.Listen.Host = host; c.Listen.Port = body.Listen.Port }); err != nil {
				writeErr(w, http.StatusInternalServerError, "save config failed: "+err.Error(), "internal_error")
				return
			}
			restartRequired = true
		}
	}
	// 日志路径热更：先切换运行态并验证新库可打开，成功后再持久化配置；
	// 若先写盘后切换，切换失败时会出现配置宣称新路径、实际仍写旧库的不一致
	newLogPath := ""
	if body.Log != nil && strings.TrimSpace(body.Log.DBPath) != "" {
		p := strings.TrimSpace(body.Log.DBPath)
		if p != s.State.Snap().LogDBPath {
			if s.OnLogPathChange != nil {
				// 路径切换持 logOpMu 写锁——不持锁，切换期间（关旧库→
				// 开新库→swap 引用）并发查询持旧引用直接 database is closed（实测可复现）。
				// 持写锁后查询引用在锁内获取，切换全程互斥；锁序与清空路径一致
				//（logOpMu → main 层 logSw.mu），无死锁风险。
				s.logOpMu.Lock()
				err := s.OnLogPathChange(p)
				s.logOpMu.Unlock()
				if err != nil {
					writeErr(w, http.StatusInternalServerError, "切换日志数据库失败: "+err.Error(), "internal_error")
					return
				}
				newLogPath = p
			} else {
				newLogPath = p // 无回调（测试环境）：仅持久化
			}
		}
	}
	// 热加载字段
	err := s.State.MutateSettings(func(ns *core.Snapshot) {
		if body.IPTrustLevel != nil {
			if *body.IPTrustLevel < 0 {
				*body.IPTrustLevel = 0
			}
			// 上限 clamp：XFF 层级实际不可能超过个位数（ExtractClientIP 越界会回退
			// 到首/末个 IP），超大值只可能是异常输入，不应进入配置与日志。
			if *body.IPTrustLevel > maxIPTrustLevel {
				*body.IPTrustLevel = maxIPTrustLevel
			}
			ns.IPTrustLevel = *body.IPTrustLevel
		}
		if body.Routing != nil {
			st := body.Routing.Strategy
			if st == "random" || st == "round_robin" || st == "weighted" {
				ns.Strategy = st
			}
		}
		if body.Cooldown != nil {
			if body.Cooldown.Enabled != nil {
				ns.CooldownEnabled = *body.Cooldown.Enabled
			}
			if body.Cooldown.DurationSeconds > 0 {
				ns.CooldownSeconds = body.Cooldown.DurationSeconds
			}
			if body.Cooldown.RetryCount > 0 {
				ns.CooldownRetryCount = body.Cooldown.RetryCount
			}
		}
		if body.UpstreamTimeoutSeconds != nil && *body.UpstreamTimeoutSeconds > 0 {
			ns.UpstreamTimeoutSeconds = *body.UpstreamTimeoutSeconds
		}
		if body.TestMessage != nil {
			ns.TestMessage = *body.TestMessage
		}
		// 默认 UA 允许清空（回退 Go 默认），TrimSpace 后纯空白视为空
		if body.DefaultUserAgent != nil {
			ns.DefaultUserAgent = strings.TrimSpace(*body.DefaultUserAgent)
		}
		if newLogPath != "" {
			ns.LogDBPath = newLogPath
		}
		// 日志记录开关（）：热加载生效，同步落盘 config.yml
		if body.Log != nil && body.Log.Enabled != nil {
			ns.LogEnabled = *body.Log.Enabled
		}
	})
	if err != nil {
		// 日志路径切换已成功但配置持久化失败——回切旧库保持三方一致
		//（：运行态写新库、配置文件/设置页显示旧路径，重启后回旧库，
		// 切换窗口期写入新库的日志“凭空消失”；旧库文件未被删除，重新打开即可恢复）。
		if newLogPath != "" && s.OnLogPathChange != nil {
			oldPath := s.State.Snap().LogDBPath // MutateSettings 失败时快照未更新，仍为旧路径
			fmt.Printf("[settings] 日志路径已切换到 %s 但保存配置失败，正在回滚到 %s…\n", newLogPath, oldPath)
			// 回滚同样持写锁（与正向切换同口径）
			s.logOpMu.Lock()
			rbErr := s.OnLogPathChange(oldPath)
			s.logOpMu.Unlock()
			if rbErr != nil {
				fmt.Printf("[settings] 回滚失败：日志当前写入 %s，但配置文件仍指向 %s（重启后生效旧路径），新库文件留在磁盘可手工清理\n", newLogPath, oldPath)
			} else {
				fmt.Printf("[settings] 已回滚：日志继续写入 %s，与配置文件一致\n", oldPath)
			}
		}
		writeErr(w, http.StatusInternalServerError, "save config failed: "+err.Error(), "internal_error")
		return
	}
	resp := map[string]interface{}{"ok": true}
	if restartRequired {
		resp["warning"] = "监听地址/端口变更需重启服务后生效"
	}
	writeJSON(w, http.StatusOK, resp)
}

// ===== 冷却池 =====

func (s *Server) handleCooldownList(w http.ResponseWriter, r *http.Request) {
	items := s.State.Cooldown.List(func(id string) string {
		if p := s.State.FindProvider(id); p != nil {
			return p.Name
		}
		return id
	})
	if items == nil {
		items = []core.CooldownItem{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": items})
}

func (s *Server) handleCooldownRemove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProviderID string `json:"provider_id"`
		Model      string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ProviderID == "" || body.Model == "" {
		writeErr(w, http.StatusBadRequest, "provider_id and model required", "invalid_request")
		return
	}
	s.State.Cooldown.Remove(body.ProviderID, body.Model)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCooldownClear(w http.ResponseWriter, r *http.Request) {
	s.State.Cooldown.Clear()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleModelsList 管理端模型列表（所有启用供应商的模型名+别名，供下拉/多选）。
func (s *Server) handleModelsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"models": s.State.AllModelNames()})
}

// ===== 仪表盘 =====

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	snap := s.State.Snap()
	var pEnabled, kEnabled int
	for _, p := range snap.Providers {
		// 与 ResolveModel / AllModelNames 同口径：供应商级 enabled=true 且至少一条 API Key 可用
		// 才计为"启用"。全 Key 停用的供应商在路由层不参与候选，仪表盘若计为"启用"会让
		// 实际不可用的供应商与"启用"数字对不上，排查时产生误导。
		if p.Enabled && core.HasEnabledAPIKey(p) {
			pEnabled++
		}
	}
	for _, k := range snap.APIKeys {
		if k.Enabled {
			kEnabled++
		}
	}
	var st *logsdb.DashboardStats
	// store 引用在锁内获取，避免清空重建窗口内拿到已关旧库
	s.logOpMu.RLock()
	store := s.LogStore()
	if store == nil {
		s.logOpMu.RUnlock()
		st = &logsdb.DashboardStats{}
	} else {
		// 与 statistics/日志接口同口径的本地时区 RFC3339——手工拼接
		// 无时区后缀的 YYYY-MM-DDT00:00:00，当前恰好因字典序巧合正确，但绕过全局
		// 归一化口径，timestamp 存储格式任何调整时这处裸格式会静默错乱且难以排查。
		now := time.Now()
		todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Format(time.RFC3339)
		var err error
		st, err = store.Dashboard(todayStart)
		s.logOpMu.RUnlock()
		// closed 重试（热加载路径切换兑底，同 handleLogsList）
		if err != nil && isClosedDBErr(err) {
			s.logOpMu.RLock()
			if ns := s.LogStore(); ns != nil && ns != store {
				st, err = ns.Dashboard(todayStart)
			}
			s.logOpMu.RUnlock()
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "查询统计失败: "+err.Error(), "internal_error")
			return
		}
	}
	successRate := 0.0
	if st.TotalCalls > 0 {
		successRate = float64(st.SuccessCalls) / float64(st.TotalCalls)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"providers":    map[string]int{"enabled": pEnabled, "total": len(snap.Providers)},
		"apikeys":      map[string]int{"enabled": kEnabled, "total": len(snap.APIKeys)},
		"today_calls":  st.TodayCalls,
		"total_calls":  st.TotalCalls,
		"success_rate": successRate,
		"tokens": map[string]int64{
			"prompt":     st.PromptTokens,
			"completion": st.CompletionTokens,
			"total":      st.TotalTokens,
		},
		"status":                   "running",
		"listen":                   s.Listen,
		"routing_strategy":         snap.Strategy,
		"cooldown_pool_size":       s.State.Cooldown.Size(),
		"upstream_timeout_seconds": snap.UpstreamTimeoutSeconds,
	})
}

// ===== 统计 =====

func (s *Server) handleStatistics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rangeType := q.Get("range")
	if rangeType == "" {
		rangeType = "7d"
	}
	now := time.Now()
	var start, end time.Time
	switch rangeType {
	case "today":
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		end = now
	case "7d":
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -6)
		end = now
	case "30d":
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -29)
		end = now
	case "custom":
		cs, ce := q.Get("custom_start"), q.Get("custom_end")
		var err error
		start, err = time.ParseInLocation("2006-01-02", cs, now.Location())
		if err != nil {
			writeErr(w, http.StatusBadRequest, "custom_start 需为 YYYY-MM-DD", "invalid_request")
			return
		}
		end, err = time.ParseInLocation("2006-01-02", ce, now.Location())
		if err != nil {
			writeErr(w, http.StatusBadRequest, "custom_end 需为 YYYY-MM-DD", "invalid_request")
			return
		}
		// 反向区间直接 400——静默落入空区间返回全零数据，
		// 用户会误判为服务故障。
		if start.After(end) {
			writeErr(w, http.StatusBadRequest, "custom_start 不能晚于 custom_end", "invalid_request")
			return
		}
		end = end.AddDate(0, 0, 1).Add(-time.Second) // 含当日
	default:
		writeErr(w, http.StatusBadRequest, "range 需为 today/7d/30d/custom", "invalid_request")
		return
	}
	if end.After(now) {
		end = now
	}
	// end 被 clamp 到 now 后 start 仍可能在未来（用户误选全未来区间）——
	// 此时形成空区间静默返回全零，用户会误判为服务故障/日志丢失；与 F12 同动机补 400。
	if start.After(now) {
		writeErr(w, http.StatusBadRequest, "custom_start 不能晚于当前时间", "invalid_request")
		return
	}
	startStr := start.Format(time.RFC3339)
	endStr := end.Format(time.RFC3339)
	dimension := q.Get("dimension")
	// 非法 dimension 直接 400——静默返回空对象，前端拼写错误与
	// 「区间内确实无数据」无法区分，图表全空白容易被误判为服务故障。
	switch dimension {
	case "", "timeline", "provider", "model", "apikey", "apikey_detail", "summary":
	default:
		writeErr(w, http.StatusBadRequest, "dimension 需为 timeline/provider/model/apikey/apikey_detail/summary", "invalid_request")
		return
	}

	store := s.LogStore()
	if store == nil {
		writeErr(w, http.StatusInternalServerError, "日志数据库不可用", "internal_error")
		return
	}

	need := func(d string) bool { return dimension == "" || dimension == d }

	// store 引用在 logOpMu 读锁内获取，全部维度查询持锁执行；
	// 遇库关闭错误时重取引用整体重试一次，
	// 各维度查询为纯读、幂等，整体重跑无副作用。
	runAll := func(store *logsdb.LogStore) (map[string]interface{}, error) {
		result := map[string]interface{}{"range": rangeType}
		if need("timeline") {
			prefix := 10
			if rangeType == "today" {
				prefix = 13 // 今日按小时
			}
			tl, err := store.Timeline(startStr, endStr, prefix)
			if err != nil {
				return nil, err
			}
			if tl == nil {
				tl = []logsdb.TimelinePoint{}
			}
			result["timeline"] = tl
		}
		if need("provider") {
			rows, err := store.GroupBy(startStr, endStr, "provider")
			if err != nil {
				return nil, err
			}
			out := []map[string]interface{}{}
			for _, row := range rows {
				out = append(out, map[string]interface{}{"provider": row.Key, "calls": row.Calls, "tokens": row.Tokens, "success_rate": row.SuccessRate})
			}
			result["by_provider"] = out
		}
		if need("model") {
			rows, err := store.GroupBy(startStr, endStr, "model")
			if err != nil {
				return nil, err
			}
			out := []map[string]interface{}{}
			for _, row := range rows {
				out = append(out, map[string]interface{}{"model": row.Key, "calls": row.Calls, "tokens": row.Tokens, "success_rate": row.SuccessRate})
			}
			result["by_model"] = out
		}
		if need("apikey") {
			rows, err := store.GroupBy(startStr, endStr, "apikey")
			if err != nil {
				return nil, err
			}
			out := []map[string]interface{}{}
			for _, row := range rows {
				// by_apikey 维度按明文 Key 分组但输出必须脱敏（与日志列表同口径），
				// 避免完整代理 Key 经统计接口泄露
				out = append(out, map[string]interface{}{"apikey": core.MaskKey(row.Key), "apikey_name": row.Label, "calls": row.Calls, "tokens": row.Tokens, "success_rate": row.SuccessRate})
			}
			result["by_apikey"] = out
		}
		if need("apikey_detail") {
			rows, err := store.GroupBy(startStr, endStr, "apikey_id")
			if err != nil {
				return nil, err
			}
			out := []map[string]interface{}{}
			for _, row := range rows {
				out = append(out, map[string]interface{}{
					"apikey_id": row.Key, "apikey_name": row.Label, "calls": row.Calls,
					"success": row.Success, "failure": row.Failure, "tokens": row.Tokens, "avg_duration_ms": row.AvgDurationMs,
				})
			}
			result["by_apikey_detail"] = out
		}
		if need("summary") {
			sm, err := store.Summary(startStr, endStr)
			if err != nil {
				return nil, err
			}
			result["summary"] = sm
		}
		return result, nil
	}
	s.logOpMu.RLock()
	store2 := s.LogStore()
	var result map[string]interface{}
	var err error
	if store2 != nil {
		result, err = runAll(store2)
	}
	s.logOpMu.RUnlock()
	if err != nil && isClosedDBErr(err) {
		s.logOpMu.RLock()
		if ns := s.LogStore(); ns != nil && ns != store2 {
			result, err = runAll(ns)
		}
		s.logOpMu.RUnlock()
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "统计查询失败: "+err.Error(), "internal_error")
		return
	}
	if result == nil {
		result = map[string]interface{}{"range": rangeType}
	}
	writeJSON(w, http.StatusOK, result)
}
