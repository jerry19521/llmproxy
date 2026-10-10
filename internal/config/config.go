// Package config 负责 LLMProxy 的配置定义、加载、保存、自动生成与热加载。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// ProviderAPIKey 供应商 API Key（代理调用上游时使用）。
type ProviderAPIKey struct {
	ID  string `yaml:"id" json:"id"`
	Key string `yaml:"key" json:"key"`
}

// ProviderHeader 供应商自定义请求头。
type ProviderHeader struct {
	Key   string `yaml:"key" json:"key"`
	Value string `yaml:"value" json:"value"`
}

// ProviderModel 供应商模型定义。
type ProviderModel struct {
	Name    string   `yaml:"name" json:"name"`
	Aliases []string `yaml:"aliases,omitempty" json:"aliases,omitempty"`
}

// Provider 模型供应商。
type Provider struct {
	ID        string           `yaml:"id" json:"id"`
	Name      string           `yaml:"name" json:"name"`
	BaseURL   string           `yaml:"base_url" json:"base_url"`
	APIType   string           `yaml:"api_type,omitempty" json:"api_type,omitempty"` // openai(默认) | responses | anthropic
	Enabled   bool             `yaml:"enabled" json:"enabled"`
	Weight    int              `yaml:"weight" json:"weight"`
	APIKeys   []ProviderAPIKey `yaml:"api_keys" json:"api_keys"`
	Headers   []ProviderHeader `yaml:"headers" json:"headers"`
	Models    []ProviderModel  `yaml:"models" json:"models"`
	CreatedAt string           `yaml:"created_at" json:"created_at"`
	UpdatedAt string           `yaml:"updated_at" json:"updated_at"`
}

// ProxyKeyQuota 额度结构（预留，本次不实现逻辑）。
type ProxyKeyQuota struct {
	Period   string `yaml:"period,omitempty" json:"period,omitempty"`       // day | month | total
	TokenMax int64  `yaml:"token_max,omitempty" json:"token_max,omitempty"` // token 额度
	CallMax  int64  `yaml:"call_max,omitempty" json:"call_max,omitempty"`   // 请求次数额度
}

// ProxyKey 代理 API Key（终端用户调用本代理的凭证）。
type ProxyKey struct {
	ID          string         `yaml:"id" json:"id"`
	Name        string         `yaml:"name" json:"name"`
	Key         string         `yaml:"key" json:"key"`
	ExpiresAt   string         `yaml:"expires_at,omitempty" json:"expires_at,omitempty"` // ISO8601，空=永久
	IPWhitelist []string       `yaml:"ip_whitelist" json:"ip_whitelist"`
	Enabled     bool           `yaml:"enabled" json:"enabled"`
	Models      []string       `yaml:"models" json:"models"`                   // 空=不限制
	Quota       *ProxyKeyQuota `yaml:"quota,omitempty" json:"quota,omitempty"` // 预留字段
	CreatedAt   string         `yaml:"created_at" json:"created_at"`
	UpdatedAt   string         `yaml:"updated_at" json:"updated_at"`
}

// Listen 监听配置。
type Listen struct {
	Host string `yaml:"host" json:"host"`
	Port int    `yaml:"port" json:"port"`
}

// Admin 管理员配置。
type Admin struct {
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"-"` // bcrypt 哈希
}

// JWTConfig JWT 配置。
type JWTConfig struct {
	Secret      string `yaml:"secret" json:"secret"`
	ExpireHours int    `yaml:"expire_hours" json:"expire_hours"`
}

// normalize 归一化 JWT 配置（v1.0.0，L2 修复）：expire_hours 为 0/负值时回退 24。
// 否则手写 yml 配 expire_hours: 0 会导致登录成功但 token 即刻过期（签发即失效），
// 管理员被锁在门外且无任何提示。与 CooldownConfig.normalize 同模式兜底。
func (c *JWTConfig) normalize() {
	if c.ExpireHours <= 0 {
		c.ExpireHours = 24
	}
}

// LogConfig 日志配置。
type LogConfig struct {
	DBPath  string `yaml:"db_path" json:"db_path"`
	Enabled *bool  `yaml:"enabled,omitempty" json:"-"` // 是否记录调用日志（v1.0.0）：nil（旧配置无该字段）视为 false，默认不记录
}

// EnabledVal 返回是否记录调用日志（nil 视为 false：默认关闭，含旧配置升级场景）。
func (c *LogConfig) EnabledVal() bool { return c.Enabled != nil && *c.Enabled }

// RoutingConfig 路由策略配置。
type RoutingConfig struct {
	Strategy string `yaml:"strategy" json:"strategy"` // random | round_robin | weighted
}

// CooldownConfig 冷却池配置。
// 兼容 v1.5 及更早配置：旧字段 duration_minutes（分钟）读取后自动换算为 duration_seconds，
// 保存时不再输出旧字段；旧配置无 enabled 字段时默认启用。
type CooldownConfig struct {
	Enabled         *bool `yaml:"enabled,omitempty" json:"-"`
	DurationSeconds int   `yaml:"duration_seconds" json:"duration_seconds"`
	RetryCount      int   `yaml:"retry_count" json:"retry_count"`      // 连续失败多少轮后才入冷却池
	DurationMinutes int   `yaml:"duration_minutes,omitempty" json:"-"` // 旧版字段：仅迁移读取，保存时不再输出
}

// EnabledVal 返回冷却池是否启用（nil 视为启用）。
func (c *CooldownConfig) EnabledVal() bool { return c.Enabled == nil || *c.Enabled }

// normalize 归一化冷却池配置：旧字段迁移、缺省值兜底。在配置加载与热加载时调用。
// 注意：调用前需将 Cooldown 置零后再解码（见 Load/Watch），否则 Default 值会干扰旧字段迁移判断。
func (c *CooldownConfig) normalize() {
	if c.DurationSeconds <= 0 {
		if c.DurationMinutes > 0 {
			c.DurationSeconds = c.DurationMinutes * 60 // 旧配置迁移：分钟 → 秒
		} else {
			c.DurationSeconds = 300
		}
	}
	c.DurationMinutes = 0 // 保存时不再输出旧字段
	if c.RetryCount <= 0 {
		c.RetryCount = 1
	}
	if c.Enabled == nil {
		e := true
		c.Enabled = &e
	}
}

// Config 顶层配置，对应 config.yml。
type Config struct {
	Listen                 Listen         `yaml:"listen" json:"listen"`
	IPTrustLevel           int            `yaml:"ip_trust_level" json:"ip_trust_level"`
	Admin                  Admin          `yaml:"admin" json:"-"`
	JWT                    JWTConfig      `yaml:"jwt" json:"-"`
	Log                    LogConfig      `yaml:"log" json:"log"`
	Routing                RoutingConfig  `yaml:"routing" json:"routing"`
	Cooldown               CooldownConfig `yaml:"cooldown" json:"cooldown"`
	UpstreamTimeoutSeconds int            `yaml:"upstream_timeout_seconds" json:"upstream_timeout_seconds"`
	TestMessage            string         `yaml:"test_message" json:"test_message"`
	DefaultUserAgent       string         `yaml:"default_user_agent" json:"default_user_agent"` // 发往上游的默认 User-Agent（v1.0.0）；空=不设置（用 Go 默认）；供应商自定义头可覆盖
	Providers              []*Provider    `yaml:"providers" json:"providers"`
	APIKeys                []*ProxyKey    `yaml:"apikeys" json:"apikeys"`
}

// Manager 管理配置文件的加载、保存与热加载检测。
// 并发模型（v1.0.0 起）：m.cfg 一经发布即不可变——Update 作用在浅拷贝上，
// Watch 整体替换指针；Cfg() 返回的旧对象后续不会被原地修改，
// 管理接口的并发读（requireAuth/handleLogin 等）与写不存在数据竞争。
type Manager struct {
	mu             sync.Mutex
	path           string
	cfg            *Config
	lastMod        time.Time
	lastSize       int64
	onChange       func(*Config) // 热加载回调
	stopCh         chan struct{}
	listenOverride *Listen // 启动参数 -host/-port 覆盖（nil=无）：仅影响运行值，不落盘
}

// NewManager 创建配置管理器。
func NewManager(path string) *Manager {
	return &Manager{path: path, stopCh: make(chan struct{})}
}

// SetOnChange 设置热加载回调（配置文件外部变更时触发，listen/jwt 除外）。
func (m *Manager) SetOnChange(fn func(*Config)) { m.onChange = fn }

// Path 返回配置文件路径。
func (m *Manager) Path() string { return m.path }

// SetListenOverride 设置启动参数对监听地址/端口的覆盖。
// 覆盖值仅影响运行时的监听与管理界面显示，不会写回配置文件
// （避免临时性 -port/-host 参数经后续任意保存被意外持久化，见需求 9.3）。
func (m *Manager) SetListenOverride(host string, port int) {
	m.mu.Lock()
	m.listenOverride = &Listen{Host: host, Port: port}
	m.mu.Unlock()
}

// EffectiveListen 返回实际生效的监听配置：启动参数覆盖优先，其次配置文件值。
func (m *Manager) EffectiveListen() Listen {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listenOverride != nil {
		return *m.listenOverride
	}
	return m.cfg.Listen
}

// Default 返回默认配置。
func Default() *Config {
	return &Config{
		Listen:                 Listen{Host: "0.0.0.0", Port: 9080},
		IPTrustLevel:           0,
		Admin:                  Admin{Username: "admin", Password: ""},
		JWT:                    JWTConfig{Secret: "", ExpireHours: 24},
		Log:                    LogConfig{DBPath: "data.db", Enabled: BoolPtr(false)},
		Routing:                RoutingConfig{Strategy: "random"},
		Cooldown:               CooldownConfig{Enabled: BoolPtr(true), DurationSeconds: 300, RetryCount: 1},
		UpstreamTimeoutSeconds: 300,
		TestMessage:            "hi",
		Providers:              []*Provider{},
		APIKeys:                []*ProxyKey{},
	}
}

// BoolPtr 返回 bool 指针（供快照写回配置时取地址）。
func BoolPtr(v bool) *bool { return &v }

// nowRFC3339 当前本地时间（ISO 8601 带时区偏移）。
func nowRFC3339() string { return time.Now().Format(time.RFC3339) }

// randomString 生成 n 位随机字母数字字符串。
// 采用拒绝采样（rejection sampling）：字节值 ≥ 248（62×4，保证整除均匀）时丢弃重取，
// 消除朴素取模 256%62=8 带来的前 8 字符概率偏高（约 +1.6%）问题。
func randomString(n int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const limit = 248 // 62 的最大整数倍且 ≤ 256
	out := make([]byte, 0, n)
	buf := make([]byte, 64)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			// 熵源失败退化路径（极端环境），保持可用性。
			// v1.0.0（S8）：旧退化路径以全零 buf[0] 为起点逐字符递增，产出 'a','b','c'…
			// 完全可预测的顺序凭证；改用时间熵驱动的线性同余序列，虽不及 CSPRNG
			// 但不再源码级可预测。
			state := time.Now().UnixNano() ^ int64(os.Getpid())<<32
			for len(out) < n {
				state = state*6364136223846793005 + 1442695040888963407
				out = append(out, charset[int(uint64(state)>>33)%len(charset)])
			}
			break
		}
		for _, c := range buf {
			if int(c) >= limit {
				continue
			}
			out = append(out, charset[int(c)%len(charset)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out)
}

// RandomProxyKey 生成 sk- 前缀的 32 位随机代理 Key。
func RandomProxyKey() string { return "sk-" + randomString(32) }

// GenerateInitialCredentials 在配置为空时自动生成管理员密码与 JWT secret。
// 返回：明文初始密码（仅密码生成时非空）、是否有任何凭证被生成、错误。
func (c *Config) GenerateInitialCredentials() (plainPassword string, changed bool, err error) {
	if c.Admin.Password == "" {
		plainPassword = randomString(12)
		hash, err := bcrypt.GenerateFromPassword([]byte(plainPassword), bcrypt.DefaultCost)
		if err != nil {
			return "", false, fmt.Errorf("生成密码哈希失败: %w", err)
		}
		c.Admin.Password = string(hash)
		changed = true
	}
	if c.JWT.Secret == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", false, fmt.Errorf("生成 JWT secret 失败: %w", err)
		}
		c.JWT.Secret = hex.EncodeToString(b)
		changed = true
	}
	return plainPassword, changed, nil
}

// Load 加载配置；文件不存在时生成含默认值的配置文件。
func (m *Manager) Load() (*Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := os.ReadFile(m.path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}
		cfg := Default()
		pwd, _, err := cfg.GenerateInitialCredentials()
		if err != nil {
			return nil, err
		}
		if err := m.saveLocked(cfg); err != nil {
			return nil, err
		}
		if pwd != "" {
			fmt.Printf("\n========================================\n")
			fmt.Printf("  首次启动：已自动生成管理员初始密码\n")
			fmt.Printf("  用户名: %s\n", cfg.Admin.Username)
			fmt.Printf("  密  码: %s\n", pwd)
			fmt.Printf("  （请尽快登录后修改密码）\n")
			fmt.Printf("========================================\n\n")
		}
		m.cfg = cfg
		m.stat()
		return cfg, nil
	}
	cfg, pwd, changed, err := parseAndPrepare(data)
	if err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	warnWeakJWTSecret(cfg) // v1.0.0（S13）：手写弱 secret 告警（仅启动时提示一次，不拒绝启动）
	m.cfg = cfg
	m.stat()
	if changed {
		// 凭证被自动生成，写回文件持久化（避免重启后丢失/重复生成）
		if err := m.saveLocked(cfg); err != nil {
			return nil, err
		}
		if pwd != "" {
			fmt.Printf("\n========================================\n")
			fmt.Printf("  检测到 admin.password 为空，已自动生成新密码\n")
			fmt.Printf("  用户名: %s\n", cfg.Admin.Username)
			fmt.Printf("  密  码: %s\n", pwd)
			fmt.Printf("========================================\n\n")
		}
	}
	return cfg, nil
}

// parseAndPrepare 解析配置内容并完成归一化与凭证自动生成（Load/Watch/AbsorbExternalChange 共用）。
// 返回：配置、明文初始密码（仅密码生成时非空）、凭证是否有变更、错误。
func parseAndPrepare(data []byte) (*Config, string, bool, error) {
	cfg := Default()
	cfg.Cooldown = CooldownConfig{} // 冷却池从零值解码：区分“未配置”与“默认值”，保障旧字段迁移
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, "", false, err
	}
	cfg.Cooldown.normalize() // 兼容旧版 duration_minutes 字段
	cfg.JWT.normalize()      // 兼容 expire_hours 0/负值（v1.0.0 L2）
	// 需求 5.2.3/9.1：手动清空 admin.password / jwt.secret 后自动重新生成
	pwd, changed, err := cfg.GenerateInitialCredentials()
	if err != nil {
		return nil, "", false, err
	}
	return cfg, pwd, changed, nil
}

// warnWeakJWTSecret 对手写弱 JWT secret 打印告警（v1.0.0 S13）：
// HS256 的安全性完全依赖 secret 强度，弱 secret 可被持有任一有效 token 的
// 攻击者离线字典爆破后伪造管理 token。不拒绝启动（避免破坏既有部署），仅提示。
func warnWeakJWTSecret(cfg *Config) {
	if len(cfg.JWT.Secret) < 32 {
		fmt.Printf("[config] 警告：jwt.secret 长度不足 32 字符，离线爆破风险高，建议更换为长随机串\n")
	}
}

// stat 记录文件 mtime 与 size（用于热加载检测）。
func (m *Manager) stat() {
	if fi, err := os.Stat(m.path); err == nil {
		m.lastMod = fi.ModTime()
		m.lastSize = fi.Size()
	}
}

// Cfg 返回当前配置快照（不可变引用；后续 Update/Watch 会发布新对象，旧对象永不被原地修改）。
func (m *Manager) Cfg() *Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// Update 在锁内执行 fn 修改配置并保存到文件。
// fn 作用于当前配置的浅拷贝（copy-on-write）：只允许修改标量字段或整体替换切片，
// 不要原地修改切片元素，以免污染已发布的旧快照。保存失败时 m.cfg 保持原值。
func (m *Manager) Update(fn func(*Config)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	nc := *m.cfg
	fn(&nc)
	if err := m.saveLocked(&nc); err != nil {
		return err
	}
	m.cfg = &nc
	return nil
}

// Save 保存当前配置到文件。
func (m *Manager) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveLocked(m.cfg)
}

// saveLocked 原子写入配置文件（调用方需持有 m.mu）。
// 注意：写入内容始终是文件层值——启动参数覆盖（listenOverride）不落盘。
// 实现上若 override 存在，覆盖 cfg.Listen 仅存在于运行值（cfg 本身不携带 override）。
func (m *Manager) saveLocked(cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	dir := filepath.Dir(m.path)
	tmp, err := os.CreateTemp(dir, ".config-*.yml.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	// v1.0.0（S12）：rename 前先 fsync——掉电/崩溃时 rename 的元数据可能先于数据块
	// 持久化，config.yml（含全部上游 Key 与 bcrypt 哈希）会损坏成空文件或半新半旧内容。
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("刷盘配置临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, m.path); err != nil {
		os.Remove(tmpName)
		return err
	}
	m.stat()
	return nil
}

// Watch 启动热加载轮询（每 2 秒检测文件变更）。
// 变更检测 → 读文件 → 解析 → 替换 m.cfg 全程在锁内完成（读文件与 YAML 解析为毫秒级，
// 锁竞争极低）：与 Update（管理界面保存）完全互斥，消除旧实现中
// "外部编辑触发 Watch 处理期间，界面并发保存被旧文件内容覆盖丢失"的 write-skew 竞态。
func (m *Manager) Watch() {
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-m.stopCh:
				return
			case <-ticker.C:
				fi, err := os.Stat(m.path)
				if err != nil {
					continue
				}
				m.mu.Lock()
				changed := !fi.ModTime().Equal(m.lastMod) || fi.Size() != m.lastSize
				if !changed {
					m.mu.Unlock()
					continue
				}
				// 先更新快照，避免重复触发；读取/解析失败时回滚（见下）
				prevMod, prevSize := m.lastMod, m.lastSize
				m.lastMod = fi.ModTime()
				m.lastSize = fi.Size()
				data, err := os.ReadFile(m.path)
				if err != nil {
					// v1.0.0（S9）：回滚 mtime 标记让下轮 tick 重试，并打印告警——
					// 旧实现已消费 mtime 且静默 continue，本次外部编辑永久错过且无日志可查
					//（与 L6 修复的“误以为改了实际没改”同类问题，失败点更靠前）。
					// 瞬时失败（编辑器原子替换中间态、文件锁）重试可自愈。
					m.lastMod, m.lastSize = prevMod, prevSize
					fmt.Printf("[hot-reload] 读取配置文件失败，保持原配置，下轮重试: %v\n", err)
					m.mu.Unlock()
					continue
				}
				cfg, pwd, credChanged, err := parseAndPrepare(data)
				if err != nil {
					// v1.0.0（L6）：解析失败不再静默——保持旧配置是安全的，但必须让用户
					// 知道热加载未生效（否则误以为改了实际没改，与可排查性要求相悖）。
					// 解析失败不回滚 mtime：内容坏重试无意义，用户修复后 mtime 再变会重新触发。
					fmt.Printf("[hot-reload] 配置解析失败，保持原配置不变: %v\n", err)
					m.mu.Unlock()
					continue
				}
				m.cfg = cfg
				if credChanged {
					// 写回文件持久化；saveLocked 会更新 lastMod，不会再次触发热加载
					if err := m.saveLocked(cfg); err != nil {
						fmt.Printf("[hot-reload] 凭证写回配置文件失败: %v\n", err)
					}
				}
				m.mu.Unlock()
				if pwd != "" {
					fmt.Printf("\n========================================\n")
					fmt.Printf("  检测到 admin.password 为空，已自动生成新密码\n")
					fmt.Printf("  用户名: %s\n", cfg.Admin.Username)
					fmt.Printf("  密  码: %s\n", pwd)
					fmt.Printf("  （请尽快登录后修改密码）\n")
					fmt.Printf("========================================\n\n")
				}
				if m.onChange != nil {
					m.onChange(cfg)
				}
			}
		}
	}()
}

// AbsorbExternalChange 吸收外部编辑产生的配置变更（v1.0.0 M7）。
// 由 State.Mutate 在写盘前调用：检测到文件自上次加载后被外部修改时，先把文件内容
// 重新加载进内存（含归一化与凭证生成），使后续基于内存快照的保存包含外部变更——
// 消除「外部编辑落盘后 2 秒内（Watch 下一 tick 前）界面保存把外部变更静默覆盖」的
// write-skew 窗口（覆盖后 lastMod 与文件 mtime 一致，Watch 永远检测不到，变更完全
// 丢失且无任何告警）。
// 文件被外部改坏（解析失败）时返回错误：调用方应拒绝保存并提示先修复配置——
// 基于内存旧状态覆盖坏文件会让用户已输入的外部修改一并丢失。
// 返回是否实际吸收了变更；无变更或文件暂时不可读时返回 (false, nil)，按内存状态保存。
func (m *Manager) AbsorbExternalChange() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fi, err := os.Stat(m.path)
	if err != nil {
		return false, nil // 文件不可访问：保持旧行为（按内存状态保存）
	}
	if fi.ModTime().Equal(m.lastMod) && fi.Size() == m.lastSize {
		return false, nil // 无外部变更
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		return false, nil // 瞬时读失败：不阻塞保存（Watch 下轮 tick 会重试吸收）
	}
	cfg, pwd, credChanged, err := parseAndPrepare(data)
	if err != nil {
		return false, fmt.Errorf("配置文件已被外部修改且无法解析，请先修复后再保存: %w", err)
	}
	m.lastMod = fi.ModTime()
	m.lastSize = fi.Size()
	m.cfg = cfg
	if credChanged {
		// 外部清空了凭证：写回持久化（saveLocked 更新 lastMod，不会触发 Watch 重复加载）
		if err := m.saveLocked(cfg); err != nil {
			fmt.Printf("[config] 吸收外部变更时凭证写回失败: %v\n", err)
		}
	}
	if pwd != "" {
		fmt.Printf("[config] 外部修改清空了 admin.password，已自动生成新密码: %s（请尽快登录后修改）\n", pwd)
	}
	return true, nil
}

// Stop 停止热加载轮询（幂等：重复调用不会 panic，与 core.State.Stop 同防护风格，v1.0.0 L4）。
func (m *Manager) Stop() {
	select {
	case <-m.stopCh:
		// 已关闭
	default:
		close(m.stopCh)
	}
}
