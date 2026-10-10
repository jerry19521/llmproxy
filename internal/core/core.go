// Package core 维护代理运行时状态：供应商与代理 Key 的内存快照、模型别名解析、
// 路由策略（随机/轮询/加权）、失败熔断冷却池。
// 架构：config.Manager 是配置文件权威（读写 yml）；State 提供 copy-on-write
// 不可变快照供代理数据面无锁读取；所有写操作经 State.Mutate 同步内存与 yml。
package core

import (
	"crypto/rand"
	"math/big"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"llmproxy/internal/config"
)

// Snapshot 运行时不可变快照（copy-on-write，读取无锁）。
// 注意：监听地址/管理员/JWT 归 config.Manager 管理，不进入快照。
type Snapshot struct {
	Providers              []*config.Provider
	APIKeys                []*config.ProxyKey
	Strategy               string
	CooldownEnabled        bool
	CooldownSeconds        int
	CooldownRetryCount     int
	UpstreamTimeoutSeconds int
	TestMessage            string
	DefaultUserAgent       string // 发往上游的默认 User-Agent（v1.8.6）：空=不设置（Go 默认）；供应商自定义头优先
	IPTrustLevel           int
	LogDBPath              string
	LogEnabled             bool // 是否记录新的调用日志（v1.8.1）：false 时代理不写日志，已有日志仍可查询
}

// CooldownItem 冷却池条目。
type CooldownItem struct {
	ProviderID   string    `json:"provider_id"`
	ProviderName string    `json:"provider_name"`
	Model        string    `json:"model"`
	Until        time.Time `json:"until"`
}

// CooldownPool (供应商, 模型) 级冷却池。
// 失败计数：同一 (供应商, 模型) 连续“全部 Key 失败”达到重试次数阈值后才真正入池；
// 期间任一次成功调用（或手动移除/清空）将计数清零。
type CooldownPool struct {
	mu         sync.Mutex
	items      map[string]time.Time // key: providerID|model → 冷却截止时间
	failStreak map[string]int       // key: providerID|model → 连续失败轮数（未达阈值时累计）
}

// NewCooldownPool 创建冷却池。
func NewCooldownPool() *CooldownPool {
	return &CooldownPool{items: map[string]time.Time{}, failStreak: map[string]int{}}
}

func cooldownKey(providerID, model string) string { return providerID + "|" + model }

// Add 将 (provider, model) 加入冷却池（seconds 秒后自动恢复）。
func (p *CooldownPool) Add(providerID, model string, seconds int) {
	if seconds <= 0 {
		seconds = 300
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.items[cooldownKey(providerID, model)] = time.Now().Add(time.Duration(seconds) * time.Second)
}

// RecordFailure 记录一轮“该供应商该模型全部 Key 失败”。
// 连续失败轮数达到 threshold（重试次数，<=1 时视为一轮即冷却）时加入冷却池并返回 true。
func (p *CooldownPool) RecordFailure(providerID, model string, seconds, threshold int) bool {
	if threshold < 1 {
		threshold = 1
	}
	key := cooldownKey(providerID, model)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failStreak[key]++
	if p.failStreak[key] >= threshold {
		delete(p.failStreak, key)
		if seconds <= 0 {
			seconds = 300
		}
		p.items[key] = time.Now().Add(time.Duration(seconds) * time.Second)
		return true
	}
	return false
}

// RecordSuccess 记录一次成功调用，重置该 (供应商, 模型) 的连续失败计数。
func (p *CooldownPool) RecordSuccess(providerID, model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.failStreak, cooldownKey(providerID, model))
}

// cleanup 清理已到期条目（调用方需持有 p.mu）。
func (p *CooldownPool) cleanup() {
	now := time.Now()
	for k, until := range p.items {
		if now.After(until) {
			delete(p.items, k)
		}
	}
}

// StartJanitor 启动后端 30 秒定时清理（需求 3.4：后端每 30 秒定时清理已到期条目）。
func (p *CooldownPool) StartJanitor(stopCh <-chan struct{}) {
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-t.C:
				p.mu.Lock()
				p.cleanup()
				p.mu.Unlock()
			}
		}
	}()
}

// IsCooling 判断是否冷却中。
func (p *CooldownPool) IsCooling(providerID, model string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.items[cooldownKey(providerID, model)]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(p.items, cooldownKey(providerID, model))
		return false
	}
	return true
}

// Remove 手动移除条目（同时清零连续失败计数）。
func (p *CooldownPool) Remove(providerID, model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.items, cooldownKey(providerID, model))
	delete(p.failStreak, cooldownKey(providerID, model))
}

// Clear 清空冷却池（含连续失败计数）。
func (p *CooldownPool) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.items = map[string]time.Time{}
	p.failStreak = map[string]int{}
}

// List 返回冷却中条目（自动清理到期项）。
func (p *CooldownPool) List(providerName func(id string) string) []CooldownItem {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var out []CooldownItem
	for k, until := range p.items {
		if now.After(until) {
			delete(p.items, k)
			continue
		}
		parts := strings.SplitN(k, "|", 2)
		if len(parts) != 2 {
			continue // v1.8.2（C3）：防御顺序修正——先判长再取值，畸形键不 panic
		}
		item := CooldownItem{ProviderID: parts[0], Model: parts[1], Until: until}
		if providerName != nil {
			item.ProviderName = providerName(parts[0])
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Until.Before(out[j].Until) })
	return out
}

// Size 当前冷却条目数。
func (p *CooldownPool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	n := 0
	for k, until := range p.items {
		if now.After(until) {
			delete(p.items, k)
			continue
		}
		n++
	}
	return n
}

// State 代理核心状态（copy-on-write 快照 + 冷却池）。
type State struct {
	mgr         *config.Manager
	snap        atomic.Value // *Snapshot
	writeMu     sync.Mutex
	Cooldown    *CooldownPool
	janitorStop chan struct{}

	rrMu      sync.Mutex
	rrCounter map[string]uint64 // per-model 轮询计数
}

// NewState 创建状态并从配置构建快照。
func NewState(mgr *config.Manager) *State {
	s := &State{
		mgr:         mgr,
		Cooldown:    NewCooldownPool(),
		janitorStop: make(chan struct{}),
		rrCounter:   map[string]uint64{},
	}
	s.Reload()
	s.Cooldown.StartJanitor(s.janitorStop) // 冷却池 30 秒定时清理
	return s
}

// Stop 停止后台任务（冷却池清理器）。
func (s *State) Stop() {
	select {
	case <-s.janitorStop:
		// 已关闭
	default:
		close(s.janitorStop)
	}
}

// Snap 读取当前快照。
func (s *State) Snap() *Snapshot { return s.snap.Load().(*Snapshot) }

// Reload 从当前配置重建快照（启动 / 热加载）。
// 热加载例外：监听地址、端口、jwt.secret、admin 由 Manager 管理，天然不受影响。
func (s *State) Reload() {
	cfg := s.mgr.Cfg()
	ns := &Snapshot{
		Providers:              cloneProviders(cfg.Providers),
		APIKeys:                cloneAPIKeys(cfg.APIKeys),
		Strategy:               cfg.Routing.Strategy,
		CooldownEnabled:        cfg.Cooldown.EnabledVal(),
		CooldownSeconds:        cfg.Cooldown.DurationSeconds,
		CooldownRetryCount:     cfg.Cooldown.RetryCount,
		UpstreamTimeoutSeconds: cfg.UpstreamTimeoutSeconds,
		TestMessage:            cfg.TestMessage,
		DefaultUserAgent:       cfg.DefaultUserAgent,
		IPTrustLevel:           cfg.IPTrustLevel,
		LogDBPath:              cfg.Log.DBPath,
		LogEnabled:             cfg.Log.EnabledVal(),
	}
	s.snap.Store(ns)
}

// snapToConfig 将快照字段写入配置（供 Mutate 持久化）。
func (ns *Snapshot) snapToConfig(cfg *config.Config) {
	cfg.Providers = ns.Providers
	cfg.APIKeys = ns.APIKeys
	cfg.Routing.Strategy = ns.Strategy
	cfg.Cooldown.Enabled = config.BoolPtr(ns.CooldownEnabled)
	cfg.Cooldown.DurationSeconds = ns.CooldownSeconds
	cfg.Cooldown.RetryCount = ns.CooldownRetryCount
	cfg.Cooldown.DurationMinutes = 0 // 已迁移为秒
	cfg.UpstreamTimeoutSeconds = ns.UpstreamTimeoutSeconds
	cfg.TestMessage = ns.TestMessage
	cfg.DefaultUserAgent = ns.DefaultUserAgent
	cfg.IPTrustLevel = ns.IPTrustLevel
	cfg.Log.DBPath = ns.LogDBPath
	cfg.Log.Enabled = config.BoolPtr(ns.LogEnabled) // 保存时显式落盘（false 也写出，避免旧库缺字段歧义）
}

// Mutate 在锁内对快照副本执行 mutator，提交新快照并同步写 config.yml。
func (s *State) Mutate(mutator func(*Snapshot)) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// v1.8.4（M7）：保存前吸收外部编辑的配置变更——外部编辑落盘后、Watch 下一
	// tick（≤2 秒）之前的窗口内执行界面保存，旧实现基于内存旧快照全量写盘，
	// 会把外部变更静默覆盖且 lastMod 随之对齐（Watch 永远检测不到，完全丢失）。
	// 吸收后 Reload 重建快照，本次界面变更叠加在外部变更之上。
	// 文件被外部改坏时拒绝保存并上抛错误（前端提示用户先修复配置文件）。
	absorbed, err := s.mgr.AbsorbExternalChange()
	if err != nil {
		return err
	}
	if absorbed {
		s.Reload()
	}
	old := s.Snap()
	ns := &Snapshot{
		Providers:              cloneProviders(old.Providers),
		APIKeys:                cloneAPIKeys(old.APIKeys),
		Strategy:               old.Strategy,
		CooldownEnabled:        old.CooldownEnabled,
		CooldownSeconds:        old.CooldownSeconds,
		CooldownRetryCount:     old.CooldownRetryCount,
		UpstreamTimeoutSeconds: old.UpstreamTimeoutSeconds,
		TestMessage:            old.TestMessage,
		IPTrustLevel:           old.IPTrustLevel,
		LogDBPath:              old.LogDBPath,
		LogEnabled:             old.LogEnabled,
	}
	mutator(ns)
	if err := s.mgr.Update(ns.snapToConfig); err != nil {
		return err
	}
	s.snap.Store(ns)
	return nil
}

// MutateSettings 仅修改热加载设置项（不触碰 providers/apikeys）。
func (s *State) MutateSettings(fn func(*Snapshot)) error {
	return s.Mutate(func(ns *Snapshot) { fn(ns) })
}

func cloneProviders(in []*config.Provider) []*config.Provider {
	out := make([]*config.Provider, len(in))
	for i, p := range in {
		pc := *p
		pc.APIKeys = append([]config.ProviderAPIKey{}, p.APIKeys...)
		pc.Headers = append([]config.ProviderHeader{}, p.Headers...)
		pc.Models = append([]config.ProviderModel{}, p.Models...)
		out[i] = &pc
	}
	return out
}

func cloneAPIKeys(in []*config.ProxyKey) []*config.ProxyKey {
	out := make([]*config.ProxyKey, len(in))
	for i, k := range in {
		kc := *k
		kc.IPWhitelist = append([]string{}, k.IPWhitelist...)
		kc.Models = append([]string{}, k.Models...)
		out[i] = &kc
	}
	return out
}

// NewID 生成 UUID。
func NewID() string { return config.NewUUID() }

// ===== 模型解析与路由 =====

// ResolveResult 模型解析结果。
type ResolveResult struct {
	RealModel  string // 第一个命中的真实模型名（向后兼容字段：日志与白名单归一参考用）
	ByAlias    bool   // 是否通过别名命中（以第一个命中为准）
	Candidates []ProviderCandidate
}

// ProviderCandidate 候选供应商与其自身命中的真实模型名。
// 同一别名在不同供应商可指向不同真实模型（如 A:4o→gpt-4o、B:4o→gpt-4o-mini），
// 发往上游的模型名必须取该供应商自己的真实名，而非全局第一个命中的名字；
// 冷却池的 (供应商, 模型) 键同理按各自真实名记录。
type ProviderCandidate struct {
	Provider *config.Provider
	Model    string // 该供应商内命中的真实模型名
}

// ResolveModel 在所有启用供应商中解析请求模型名（名称或别名，大小写不敏感）。
// 每个供应商只返回第一个命中的模型：同一供应商内多个模型共享同一别名时不会重复入候选
// （否则随机/加权策略下该供应商权重被放大）。
func (s *State) ResolveModel(requested string) *ResolveResult {
	snap := s.Snap()
	res := &ResolveResult{}
	seen := map[string]bool{} // providerID 去重
	for _, p := range snap.Providers {
		if !p.Enabled {
			continue
		}
		for _, m := range p.Models {
			hit, byAlias := "", false
			if strings.EqualFold(m.Name, requested) {
				hit = m.Name
			} else {
				for _, alias := range m.Aliases {
					if strings.EqualFold(alias, requested) {
						hit = m.Name
						byAlias = true
						break
					}
				}
			}
			if hit == "" {
				continue
			}
			if res.RealModel == "" {
				res.RealModel = m.Name
				res.ByAlias = byAlias
			}
			if !seen[p.ID] {
				seen[p.ID] = true
				res.Candidates = append(res.Candidates, ProviderCandidate{Provider: p, Model: hit})
			}
			break // 每供应商取第一个命中的模型
		}
	}
	return res
}

// FilterCooldown 从候选中排除冷却中的 (供应商, 模型)（按各候选自身命中的模型名）；冷却池禁用时原样返回。
func (s *State) FilterCooldown(candidates []ProviderCandidate) []ProviderCandidate {
	if !s.Snap().CooldownEnabled {
		return candidates
	}
	out := make([]ProviderCandidate, 0, len(candidates))
	for _, c := range candidates {
		if !s.Cooldown.IsCooling(c.Provider.ID, c.Model) {
			out = append(out, c)
		}
	}
	return out
}

// randIntn 返回 [0,n) 随机数。
func randIntn(n int) int {
	if n <= 1 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return int(time.Now().UnixNano() % int64(n))
	}
	return int(v.Int64())
}

// PickProvider 按全局路由策略在候选中选择供应商。
// model 用于轮询策略的 per-model 计数，避免不同模型（不同候选集）共用计数器互相干扰。
func (s *State) PickProvider(candidates []ProviderCandidate, strategy, model string) *ProviderCandidate {
	if len(candidates) == 0 {
		return nil
	}
	// 按创建时间排序，保证轮询/加权分配的确定性
	sorted := append([]ProviderCandidate{}, candidates...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Provider.CreatedAt < sorted[j].Provider.CreatedAt })
	switch strategy {
	case "round_robin":
		s.rrMu.Lock()
		s.rrCounter[model]++
		n := s.rrCounter[model]
		s.rrMu.Unlock()
		return &sorted[int(n-1)%len(sorted)]
	case "weighted":
		total := 0
		for _, c := range sorted {
			w := c.Provider.Weight
			if w <= 0 {
				w = 1
			}
			total += w
		}
		r := randIntn(total)
		for _, c := range sorted {
			w := c.Provider.Weight
			if w <= 0 {
				w = 1
			}
			if r < w {
				return &c
			}
			r -= w
		}
		return &sorted[0]
	default: // random
		return &sorted[randIntn(len(sorted))]
	}
}

// PickAPIKey 在供应商内随机选择一条 API Key。
func PickAPIKey(p *config.Provider) *config.ProviderAPIKey {
	if len(p.APIKeys) == 0 {
		return nil
	}
	return &p.APIKeys[randIntn(len(p.APIKeys))]
}

// FindProvider 按 ID 查找供应商（快照）。
func (s *State) FindProvider(id string) *config.Provider {
	for _, p := range s.Snap().Providers {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// FindProxyKeyByID 按 ID 查找代理 Key。
func (s *State) FindProxyKeyByID(id string) *config.ProxyKey {
	for _, k := range s.Snap().APIKeys {
		if k.ID == id {
			return k
		}
	}
	return nil
}

// FindProxyKey 按 Key 值查找代理 Key。
func (s *State) FindProxyKey(key string) *config.ProxyKey {
	for _, k := range s.Snap().APIKeys {
		if k.Key == key {
			return k
		}
	}
	return nil
}

// MaskKey 脱敏：前 3 位 + *** + 后 3 位；短 Key（≤10 字符）只保留前 3 位（v1.8.3，L8 修复）。
// 旧实现按 `key[:3] + "***" + key[4:]` 对 7 字符 Key 会泄露 6/7 字符（后 3 与前 3 重叠）；
// 供应商上游 Key 长度由用户配置不受控，短 Key 仅保留前缀维持列表区分度、不再暴露尾部。
func MaskKey(key string) string {
	if len(key) <= 6 {
		return "***"
	}
	if len(key) <= 10 {
		return key[:3] + "***"
	}
	return key[:3] + "***" + key[len(key)-3:]
}

// ModelEntry 可用模型条目（/v1/models 与可用模型下拉数据源）。
type ModelEntry struct {
	ID       string `json:"id"`
	AliasFor string `json:"alias_for,omitempty"`
}

// AllModelNames 返回所有启用供应商的模型名称与别名（去重合并）。
func (s *State) AllModelNames() []ModelEntry {
	snap := s.Snap()
	seen := map[string]bool{}
	var out []ModelEntry
	for _, p := range snap.Providers {
		if !p.Enabled {
			continue
		}
		for _, m := range p.Models {
			if !seen[strings.ToLower(m.Name)] {
				seen[strings.ToLower(m.Name)] = true
				out = append(out, ModelEntry{ID: m.Name})
			}
			for _, a := range m.Aliases {
				if !seen[strings.ToLower(a)] {
					seen[strings.ToLower(a)] = true
					out = append(out, ModelEntry{ID: a, AliasFor: m.Name})
				}
			}
		}
	}
	return out
}
