// Package proxy 实现核心代理转发：代理 Key 认证、模型别名解析、路由选择、
// 失败重试与跨供应商降级、冷却熔断、多格式（Chat Completions / Responses / Anthropic）
// 请求转换与透传、流式边收边转发、调用日志写入。
package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"llmproxy/internal/auth"
	"llmproxy/internal/config"
	"llmproxy/internal/convert"
	"llmproxy/internal/core"
	"llmproxy/internal/logsdb"
)

// Handler 代理转发处理器。
type Handler struct {
	State  *core.State
	Logs   atomic.Value // *logsdb.LogStore，支持热更换
	client *http.Client // 共享上游客户端（连接池复用）
}

// NewHandler 创建代理处理器。
// 使用共享 http.Client 与自定义 Transport（每主机空闲连接数调大，适配代理高并发）。
func NewHandler(state *core.State) *Handler {
	return &Handler{
		State: state,
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:          200,
				MaxIdleConnsPerHost:   32,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
	}
}

// SetLogStore 替换日志存储（日志路径热更）。
func (h *Handler) SetLogStore(s *logsdb.LogStore) { h.Logs.Store(s) }

// LogStore 获取当前日志存储。
func (h *Handler) LogStore() *logsdb.LogStore {
	if v, ok := h.Logs.Load().(*logsdb.LogStore); ok {
		return v
	}
	return nil
}

// ===== 错误响应 =====

// setCORSHeaders 代理转发端点跨域头。
func setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")
}

// ServeV1 分发 /v1/* 请求（含 OPTIONS 预检处理）。
func (h *Handler) ServeV1(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}
	switch {
	case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
		h.handleModels(w, r)
	case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
		h.handleProxy(w, r, convert.FormatOpenAI)
	case r.URL.Path == "/v1/responses" && r.Method == http.MethodPost:
		h.handleProxy(w, r, convert.FormatResponses)
	case r.URL.Path == "/v1/messages" && r.Method == http.MethodPost:
		h.handleProxy(w, r, convert.FormatAnthropic)
	default:
		convert.WriteError(w, convert.FormatOpenAI, http.StatusNotFound, "invalid_request_error", "not_found", "not found")
	}
}

// ===== GET /v1/models =====

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, err := h.authenticate(w, r, convert.FormatOpenAI); err != nil {
		return
	}
	// 响应为 OpenAI /v1/models 格式的超集：
	// 额外携带 type/display_name 字段以兼容 Anthropic 客户端（多余字段 OpenAI 客户端会忽略）。
	type modelItem struct {
		ID          string `json:"id"`
		Object      string `json:"object"`
		Type        string `json:"type"`
		DisplayName string `json:"display_name"`
		AliasFor    string `json:"alias_for,omitempty"`
	}
	items := []modelItem{}
	for _, m := range h.State.AllModelNames() {
		items = append(items, modelItem{
			ID: m.ID, Object: "model", Type: "model", DisplayName: m.ID, AliasFor: m.AliasFor,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"object": "list", "data": items})
}

// ===== 认证 =====

// authenticate 校验代理 API Key 的基础项：存在、启用、有效期、IP 白名单。
// 先于请求体解析执行（需求 6.3 顺序），使无效 Key 直接 401；
// 模型白名单校验拆分至 authorizeModel（需先做别名解析得到真实模型名）。
// 错误响应按客户端格式输出；除 Authorization: Bearer 外，兼容 Anthropic 客户端的 x-api-key 头。
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request, clientFormat string) (*config.ProxyKey, error) {
	writeErr := func(status int, msg, code string) {
		convert.WriteError(w, clientFormat, status, "invalid_request_error", code, msg)
	}
	keyStr := ""
	hdr := r.Header.Get("Authorization")
	if strings.HasPrefix(hdr, "Bearer ") {
		keyStr = strings.TrimSpace(strings.TrimPrefix(hdr, "Bearer "))
	}
	if keyStr == "" {
		keyStr = strings.TrimSpace(r.Header.Get("X-Api-Key")) // Anthropic SDK 兼容
	}
	if keyStr == "" {
		writeErr(http.StatusUnauthorized, "invalid api key", "invalid_api_key")
		return nil, fmt.Errorf("missing api key")
	}
	key := h.State.FindProxyKey(keyStr)
	if key == nil || !key.Enabled {
		writeErr(http.StatusUnauthorized, "invalid api key", "invalid_api_key") // 不存在或已停用
		return nil, fmt.Errorf("invalid api key")
	}
	if key.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, key.ExpiresAt); err == nil && time.Now().After(t) {
			writeErr(http.StatusUnauthorized, "invalid api key", "invalid_api_key") // 已过期
			return nil, fmt.Errorf("api key expired")
		}
	}
	clientIP := auth.ExtractClientIP(r, h.State.Snap().IPTrustLevel)
	if !auth.MatchIPWhitelist(clientIP, key.IPWhitelist) {
		convert.WriteError(w, clientFormat, http.StatusForbidden, "access_denied", "ip_not_allowed", "ip not allowed")
		return nil, fmt.Errorf("ip not allowed")
	}
	return key, nil
}

// filterCandidatesByWhitelist 按代理 Key 可用模型白名单过滤候选供应商集合。
// 匹配口径：
//   - 未配置白名单（空）：不限制，原样返回；
//   - 客户端传入的原始模型名（requested）命中白名单：放行全部候选；
//   - 否则：仅保留「该候选自身命中的真实模型名」在白名单内的候选。多供应商共享别名时
//     （同一别名在不同供应商指向不同真实名），白名单配置非首候选的真实名不应导致合法调用
//     被整单 403，且路由只在白名单允许的候选中选择。
//
// 过滤后候选为空 → 返回 nil（调用方返回 403）。
func filterCandidatesByWhitelist(cands []core.ProviderCandidate, requested string, whitelist []string) []core.ProviderCandidate {
	if len(whitelist) == 0 {
		return cands // 未配置白名单：不限制
	}
	for _, m := range whitelist {
		if strings.EqualFold(m, requested) {
			return cands // 原始名命中：放行全部
		}
	}
	out := make([]core.ProviderCandidate, 0, len(cands))
	for _, c := range cands {
		for _, m := range whitelist {
			if strings.EqualFold(m, c.Model) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// ===== 上游请求体构建 =====

// buildUpstreamBody 构建同格式透传的上游请求体。
// 调用模型与真实模型名完全一致时，字节级原样转发：
// 通过别名或大小写差异命中时，仅替换 model 字段为真实名，其余字段原样保留。
func buildUpstreamBody(raw []byte, requested, realModel string) []byte {
	if realModel == "" || requested == realModel {
		return raw
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	m["model"], _ = json.Marshal(realModel)
	up, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return up
}

// ===== 上游请求 =====

// maxUpstreamResponseBytes 上游响应体大小上限（64MB），防御异常超大响应导致 OOM。
const maxUpstreamResponseBytes = 64 << 20

type upstreamResult struct {
	StatusCode  int
	RespHeaders http.Header
	RespBody    []byte
	// 上游请求快照（调用日志四组数据之二；认证头脱敏后记录）。
	ReqURL         string
	ReqHeaders     map[string]string
	ReqBody        []byte
	DurationMs     int64  // 总耗时：请求发出→响应体读完毕
	FirstByteMs    int64  // 首字节耗时：请求发出→响应体首字节（流式≈首 SSE 事件）；-1 表示未收到数据；流式 token 速率的起算点
	FirstContentMs int64  // 首个 content 增量耗时：请求发出→首个 content 类增量到达；-1 表示未收到 content（无 content 阶段或上游不含该字段）
	LastChunkMs    int64  // 末字节耗时：请求发出→最后一个响应块读取完成；=DurationMs 通常等价；-1 表示未收到数据
	ReadChunks     int64  // 分块到达次数：Read 到有效数据的次数；=1 表示响应体一次性到达（伪流式/缓冲型上游）
	Err            string // 非空表示请求失败（网络错误/超时等）
}

// newUpstreamRequest 构建上游 HTTP 请求（认证头按供应商接口类型设置）。
// defaultUA：全局默认 User-Agent；先于供应商自定义头应用，后者可覆盖。
func newUpstreamRequest(ctx context.Context, p *config.Provider, key, url string, body []byte, apiType, defaultUA string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	convert.SetAuthHeaders(req.Header, apiType, key)
	convert.SetDefaultUserAgent(req.Header, defaultUA)
	for _, h := range p.Headers {
		if h.Key != "" {
			req.Header.Set(h.Key, h.Value)
		}
	}
	return req, nil
}

// upstreamReqSnapshot 采集上游请求快照（日志四组数据之二；认证头脱敏）。
func upstreamReqSnapshot(req *http.Request, body []byte) (string, map[string]string, []byte) {
	return req.URL.String(), maskAuthHeaders(req.Header), body
}

// maskAuthHeaders 复制请求头并脱敏认证字段（Authorization / X-Api-Key），
// 避免上游 Key 明文落日志；其余原样保留。
func maskAuthHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		val := strings.Join(v, ", ")
		switch strings.ToLower(k) {
		case "authorization", "x-api-key":
			val = maskSecret(val)
		}
		out[k] = val
	}
	return out
}

// maskSecret 脱敏长密钥：保留前 10 与尾 4 字符（足以区分不同 Key，不泄露完整值）；
// 短值全遮蔽。
func maskSecret(s string) string {
	if len(s) <= 14 {
		return strings.Repeat("*", len(s))
	}
	return s[:10] + "..." + s[len(s)-4:]
}

// serializeHeaders 把响应头序列化为 JSON（日志用；不脱敏——响应头不含凭据）。
func serializeHeaders(h http.Header) string {
	if h == nil {
		return ""
	}
	m := make(map[string]string, len(h))
	for k, v := range h {
		m[k] = strings.Join(v, ", ")
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// callUpstream 发起一次上游请求（全缓冲：读完整响应体）；认证头按供应商接口类型设置。
// 响应体分块读取：记录首字节到达时刻（供生成耗时计算），并保留 64MB 上限。
func callUpstream(ctx context.Context, client *http.Client, p *config.Provider, key, url string, body []byte, apiType, defaultUA string) *upstreamResult {
	start := time.Now()
	req, err := newUpstreamRequest(ctx, p, key, url, body, apiType, defaultUA)
	if err != nil {
		return &upstreamResult{Err: "构建上游请求失败: " + err.Error()}
	}
	// 上游请求快照（日志四组数据之二）
	reqURL, reqHeaders, reqBody := upstreamReqSnapshot(req, body)
	resp, err := client.Do(req)
	if err != nil {
		return &upstreamResult{ReqURL: reqURL, ReqHeaders: reqHeaders, ReqBody: reqBody,
			Err: err.Error(), DurationMs: time.Since(start).Milliseconds()}
	}
	defer resp.Body.Close()
	var data []byte
	firstByte := int64(-1)
	readChunks := int64(0)
	chunk := make([]byte, 64*1024)
	for len(data) < maxUpstreamResponseBytes {
		n, rerr := resp.Body.Read(chunk)
		if n > 0 {
			if firstByte < 0 {
				firstByte = time.Since(start).Milliseconds()
			}
			readChunks++
			remain := maxUpstreamResponseBytes - int64(len(data))
			if int64(n) > remain {
				n = int(remain)
			}
			data = append(data, chunk[:n]...)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return &upstreamResult{ReqURL: reqURL, ReqHeaders: reqHeaders, ReqBody: reqBody,
				Err: "读取上游响应失败: " + rerr.Error(), StatusCode: resp.StatusCode,
				RespHeaders: resp.Header, RespBody: data,
				DurationMs: time.Since(start).Milliseconds(), FirstByteMs: firstByte, LastChunkMs: time.Since(start).Milliseconds(), ReadChunks: readChunks}
		}
	}
	return &upstreamResult{ReqURL: reqURL, ReqHeaders: reqHeaders, ReqBody: reqBody,
		StatusCode: resp.StatusCode, RespHeaders: resp.Header, RespBody: data,
		DurationMs: time.Since(start).Milliseconds(), FirstByteMs: firstByte, LastChunkMs: time.Since(start).Milliseconds(), ReadChunks: readChunks}
}

// ===== 流式转发（stream=true 边收边转发）=====

// streamAttempt 一次流式转发的结果。
type streamAttempt struct {
	result         *upstreamResult // 状态码/响应头/上游累积副本/耗时/首字节/错误
	started        bool            // 已向客户端写出数据（true 后本次调用不可重试）
	clientCanceled bool            // 客户端断开导致本次尝试失败（首字节前断开不再重试）
	clientBytes    []byte          // 发给客户端的完整字节（同格式=上游副本；跨格式=转换输出；日志 client_resp_body 用）
	sentBytes      int64           // 实际写入客户端的字节数（与 clientBytes 不同：写入中途失败时仅记已成功写出的字节）
	lastSentMs     int64           // 最后一个成功写入客户端的时刻（用于客户端断开类失败的事后定位）
	firstContentMs int64           // 首个 content 类增量到达时刻（同格式透传专用；跨格式在 StreamConverter 处识别）
	// 代理 → 客户端响应快照（日志四组数据之四）
	clientRespStatus  int    // 写给客户端的 HTTP 状态码
	clientRespHeaders string // 写给客户端的响应头快照（JSON；WriteHeader 前采集）
	err               string // 非空=失败（网络错误/非 2xx/流首事件错误/转发中断）
}

// tryStreamForward 发起一次流式上游请求并边收边转发。
// sameFmt=true 时上游 SSE 原样透传；否则经 convert.StreamConverter 实时转换为目标客户端格式。
// 延迟提交：首个有效数据到达前不向客户端写任何字节（含 WriteHeader），
// 此阶段失败（网络错误/非 2xx/流首事件为错误）可由调用方安全重试换 Key/供应商。
//
// ctx 与 clientCtx 区别：
//   - ctx 用于上游请求与 Read；必须独立于 r.Context()（Background 派生 + upstream_timeout_seconds），
//     防止客户端断开时连累上游被 cancel，导致 "读取上游响应失败: context canceled" 误归类。
//   - clientCtx 用于客户端断开检测（select clientCtx.Done() 后主动 resp.Body.Close() 中断上游读，
//     并把 att.err 改为 "客户端断开" 便于日志准确归类）。
func (h *Handler) tryStreamForward(w http.ResponseWriter, ctx, clientCtx context.Context,
	p *config.Provider, key, url string, body []byte, pType, clientFormat string,
	sameFmt bool, realModel string) *streamAttempt {

	start := time.Now()
	// 全局默认 User-Agent 随请求应用（供应商自定义头仍可在 newUpstreamRequest 内覆盖）
	req, rerr := newUpstreamRequest(ctx, p, key, url, body, pType, h.State.Snap().DefaultUserAgent)
	if rerr != nil {
		msg := "构建上游请求失败: " + rerr.Error()
		return &streamAttempt{err: msg, result: &upstreamResult{Err: msg, DurationMs: time.Since(start).Milliseconds()}}
	}
	// 上游请求快照（日志四组数据之二）
	reqURL, reqHeaders, reqBody := upstreamReqSnapshot(req, body)
	resp, err := h.client.Do(req)
	if err != nil {
		return &streamAttempt{err: err.Error(),
			result: &upstreamResult{ReqURL: reqURL, ReqHeaders: reqHeaders, ReqBody: reqBody,
				Err: err.Error(), DurationMs: time.Since(start).Milliseconds()}}
	}
	defer resp.Body.Close()

	// 客户端断开 watchdog：r.Context() cancel 时立刻关掉上游 Body 让 Read 退出循环，
	// 避免等上游自然 EOF（用户已不需要的数据）。goroutine 通过 watchdogDone 通道被告知可退出；
	// sync.Once 保证 close 只触发一次（goroutine 内已写入 resp.Body.Close 也可能同步到此通道）。
	// clientCanceled 改用 atomic.Bool——watchdog goroutine 写、主流程在流式
	// 收尾读取，普通 bool 之间无任何同步边；上游 io.EOF 正常退出与客户端断开几乎同时发生时
	// 会构成数据竞态（-race 需 CGO、Windows 无 gcc 无法运行，竞态将长期潜伏）。读写各一处，零行为变化。
	var clientCanceled atomic.Bool
	watchdogDone := make(chan struct{})
	watchdogStop := sync.Once{}
	go func() {
		select {
		case <-clientCtx.Done():
			clientCanceled.Store(true)
			_ = resp.Body.Close()
			watchdogStop.Do(func() { close(watchdogDone) })
		case <-watchdogDone:
		}
	}()
	defer watchdogStop.Do(func() { close(watchdogDone) })

	att := &streamAttempt{result: &upstreamResult{StatusCode: resp.StatusCode, RespHeaders: resp.Header,
		ReqURL: reqURL, ReqHeaders: reqHeaders, ReqBody: reqBody}}

	// 非 2xx：读错误体供重试判断（尚未向客户端写出，可安全重试）
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamResponseBytes))
		att.result.RespBody = data
		att.result.DurationMs = time.Since(start).Milliseconds()
		att.err = fmt.Sprintf("upstream HTTP %d: %s", resp.StatusCode, truncate(string(data), 500))
		return att
	}

	if sameFmt {
		passthroughStream(w, resp, att, start)
	} else {
		convertStream(w, resp, att, start, pType, clientFormat, realModel)
	}
	// 客户端断开时把 Read 阶段产生的 "读取上游响应失败: ..."（含 closed pipe / canceled 变体）统一归类为 "客户端断开"。
	// 注意：不要覆盖 w.Write 失败时的 "写入客户端失败" —— 那是更具体的客户端问题描述。
	if clientCanceled.Load() && strings.HasPrefix(att.err, "读取上游响应失败") {
		att.err = "客户端断开"
		att.result.Err = "客户端断开"
		att.clientCanceled = true // 标记断开成因，供重试决策使用
	}
	return att
}

// copyUpstreamHeaders 转发上游响应头（过滤逐跳头与长度头；流式不设 Content-Length）。
func copyUpstreamHeaders(w http.ResponseWriter, src http.Header) {
	for k, vals := range src {
		lk := strings.ToLower(k)
		if lk == "content-length" || lk == "connection" || lk == "transfer-encoding" ||
			lk == "keep-alive" || lk == "access-control-allow-origin" {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
}

// contentMarker JSON 字段名（用作"是否包含 content 类增量"的快速子串扫描目标）。
// 三种上游格式里"用户可见的输出文本"字段名各不相同，全部纳入以识别 reasoning 流中
// 何时切到 content 阶段。Anthropic 文本通过 delta.type=text_delta 隐式表示，
// 真正落到字节里是 delta.text 字段；这里只对 JSON 中的字符串字段名做匹配即可。
//
// 不包含 "reasoning"——reasoning 流中 reasoning 字段出现早于 content，若纳入会
// 导致 reasoning 阶段被误判为生成起点，无法解决 reasoning 流速率虚高的问题。
//
// 不再使用裸 `"text":` 字段名——Responses 上游的
// reasoning_summary_text.done 事件（思考摘要完成帧）JSON 同样含 "text" 字段，
// 会把 reasoning 阶段误判为 content 起点、first_content_ms 偏早；改用
// `"text_delta"`（Anthropic 文本增量事件名）与 `"type":"text"`
// （Anthropic content_block_start 的块类型声明），两者均不会在 reasoning 事件
// 中出现；已验证 thinking 帧与 reasoning_summary_text.done / reasoning_text.delta
// 均不误命中，text_delta 与 content_block_start(text) 正常命中。
var contentMarkers = []string{
	`"content":`,    // OpenAI Chat Completions: delta.content（开场 role 帧的 "content":"" 同样命中，属预期）
	`"text_delta"`,  // Anthropic Messages: content_block_delta 的 delta.type=text_delta
	`"type":"text"`, // Anthropic Messages: content_block_start 的 content_block.type=text
	`"output_text"`, // OpenAI Responses: content_part.added 的 part.type="output_text"（真实流中先于 output_text.delta 到达）
}

// streamChunkHasContent 判断累积的 SSE 字节是否已包含首个 content 类增量。
//
// 实现：取末尾若干字节（避免大响应全量扫描），遍历全部 marker 做子串匹配；
// 一旦命中任一 marker，即认为已跨过 reasoning/role 阶段、进入生成阶段。
//
// 注意：marker 是 JSON 字段名子串，OpenAI 流里 "content":""（空开场帧）也算命中——
// 这是预期行为：流式调用开场的 role chunk 就带 `"content":""` 字段，下游用此识别
// "已进入生成阶段"；reasoning 流模型（qwen3.8-flash-next 等）的开场帧同理，
// 但 reasoning 阶段不会写 `"content":` 字段，content 阶段才会。
func streamChunkHasContent(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	// 只扫最近 16KB（典型流式 chunk 长度），过深的扫描对识别"首个 content 出现"无价值
	tail := data
	if len(tail) > 16*1024 {
		tail = tail[len(tail)-16*1024:]
	}
	for _, m := range contentMarkers {
		if bytes.Contains(tail, []byte(m)) {
			return true
		}
	}
	return false
}

// streamTerminatorForwarded 判断同格式透传流中终止帧是否已成功转发给客户端。
// 三种格式的终止标记：OpenAI CC 的 "data: [DONE]"、Anthropic 的 message_stop 事件、
// Responses 的 response.completed 事件。SSE 客户端收到终止帧后即认为响应结束并关闭
// 连接（正常行为）；此时代理若还在等上游剩余字节（[DONE] 后的 keep-alive 等），
// 随后的读错误不应记为"客户端断开"失败。
func streamTerminatorForwarded(data []byte) bool {
	// 终止帧是流的最后内容，扫描尾部即可（避免大响应全量扫描）
	tail := data
	if len(tail) > 16*1024 {
		tail = tail[len(tail)-16*1024:]
	}
	return bytes.Contains(tail, []byte("data: [DONE]")) ||
		bytes.Contains(tail, []byte("data:[DONE]")) ||
		bytes.Contains(tail, []byte(`"type":"message_stop"`)) ||
		bytes.Contains(tail, []byte(`"type":"response.completed"`))
}

// passthroughStream 同格式流式透传：上游字节原样实时转发给客户端，同时累积副本（日志/统计用）。
// 副本达到 64MB 上限后仅转发不再累积（防御异常超大响应 OOM）。
//
// 顺带采集三组用于速率/可观测性的指标：
//   - FirstContentMs：首个含 content 类增量（OpenAI delta.content / Anthropic text_delta /
//     Responses output_text.delta）的 SSE 事件到达时刻；用于将 reasoning 阶段与 content
//     阶段拆分（reasoning 流模型 qwen3 / o1 等整个响应都在生成时仍回退到首字节策略）
//   - LastChunkMs：最后一个 chunk 读取完成时刻（≈DurationMs，但允许最后一块 Read 阶段有耗时）
//   - SentBytes / LastSentMs：实际写到客户端的字节数与最后写入时刻（用于 wsasend 类
//     "客户端中途断开"场景的事后定位，区分代理没发出去 vs 客户端主动断开）
func passthroughStream(w http.ResponseWriter, resp *http.Response, att *streamAttempt, start time.Time) {
	var data []byte
	firstByte := int64(-1)
	firstContent := int64(-1)
	lastChunk := int64(-1)
	readChunks := int64(0)
	started := false
	sentBytes := int64(0)
	lastSent := int64(-1)
	failPreLen := -1 // 写失败时 data 的已送达前缀长度（clientBytes 截断用）
	failWritten := 0 // 写失败块中实际送达的字节数（部分写入）
	chunk := make([]byte, 64*1024)
	for {
		n, rerr := resp.Body.Read(chunk)
		if n > 0 {
			now := time.Since(start).Milliseconds()
			if firstByte < 0 {
				firstByte = now
			}
			lastChunk = now
			readChunks++
			preLen := len(data) // 本块 append 前的长度（写失败时据此截断未送达部分）
			if int64(len(data)) < maxUpstreamResponseBytes {
				take := n
				if remain := maxUpstreamResponseBytes - int64(len(data)); int64(take) > remain {
					take = int(remain)
				}
				data = append(data, chunk[:take]...)
			}
			// 检测同时覆盖累积副本与当前块——副本达 64MB 封顶后 data 不再
			// 增长，仅扫 data 会漏检后续块中的 content 帧（first_content_ms 永远不出）；
			// data 命中历史跨块场景（帧跨 Read 边界仍可检出），chunk[:n] 覆盖封顶后的当前块。
			if firstContent < 0 && (streamChunkHasContent(data) || streamChunkHasContent(chunk[:n])) {
				firstContent = now
			}
			if !started {
				started = true
				att.started = true
				copyUpstreamHeaders(w, resp.Header)
				// 代理 → 客户端响应快照（WriteHeader 前采集；日志四组数据之四）
				att.clientRespStatus = resp.StatusCode
				att.clientRespHeaders = serializeHeaders(w.Header())
				w.WriteHeader(resp.StatusCode)
			}
			if wn, werr := w.Write(chunk[:n]); werr != nil {
				att.err = "写入客户端失败: " + werr.Error()
				failPreLen = preLen // 本块未送达，clientBytes 截断到上一块为止
				failWritten = wn    // 失败块的部分写入字节
				sentBytes += int64(wn)
				lastSent = now
				att.sentBytes = sentBytes
				att.lastSentMs = lastSent
				break
			} else {
				sentBytes += int64(n)
				lastSent = now
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if streamTerminatorForwarded(data) {
				// 终止帧已转发给客户端：响应完整送达，客户端关闭连接属正常完成，
				// 不应记为"客户端断开"失败（SSE 客户端读完终止事件即断开属正常行为）。
				break
			}
			att.err = "读取上游响应失败: " + rerr.Error()
			break
		}
	}
	att.result.RespBody = data // 上游侧副本：保留完整读取字节（含未送达的尾块）
	att.result.DurationMs = time.Since(start).Milliseconds()
	if firstByte >= 0 {
		att.result.FirstByteMs = firstByte
	}
	att.result.FirstContentMs = firstContent
	if lastChunk >= 0 {
		att.result.LastChunkMs = lastChunk
	} else {
		att.result.LastChunkMs = att.result.DurationMs
	}
	att.result.ReadChunks = readChunks
	att.result.Err = att.err
	// client_resp_body 只记已成功送达的字节（CallLog 语义：流式为已发送部分）。
	// 失败场景下若把含未送达尾块的完整上游副本记入，client_resp_body 字节数 > sent_bytes
	// 自相矛盾，排查"客户端断开时收到多少"时误导；跨格式路径本就只记成功写出块，此处对齐。
	// 失败块的部分写入字节（w.Write 返回 wn）同样计入——sentBytes 已含该
	// 部分而 clientBytes 截断到上一块会让两者口径矛盾（sent > client）；追加时先复制再拼，
	// 不在 data 底层数组上原地 append（RespBody 仍需保留完整上游副本）。
	if failPreLen >= 0 && failPreLen <= len(data) {
		att.clientBytes = append(append([]byte{}, data[:failPreLen]...), chunk[:failWritten]...)
	} else {
		att.clientBytes = data
	}
	att.sentBytes = sentBytes
	att.lastSentMs = lastSent
	att.firstContentMs = firstContent
}

// convertStream 跨格式流式转换转发：上游 SSE 事件实时转换为目标客户端格式的 SSE 输出。
// 上游流正常结束时补发收尾帧（含 usage）；流中错误事件在未开始写客户端时表现为可重试失败。
//
// 采集指标同 passthroughStream：FirstContentMs（首个文本增量到达时刻）/ SentBytes /
// LastSentMs（最后一次成功写入客户端的时刻）。跨格式路径下"首个文本增量"由 StreamConverter
// 的 evText 事件识别——转换器已经识别出 content 阶段，再回报给调用方记录时间戳。
func convertStream(w http.ResponseWriter, resp *http.Response, att *streamAttempt,
	start time.Time, pType, clientFormat, realModel string) {

	conv := convert.NewStreamConverter(pType, clientFormat, realModel)
	var data []byte        // 上游字节累积副本（usage 提取/日志）
	var clientBytes []byte // 发给客户端的转换输出（日志 resp_body）
	firstByte := int64(-1)
	firstContent := int64(-1)
	lastChunk := int64(-1)
	readChunks := int64(0)
	started := false
	sentBytes := int64(0)
	lastSent := int64(-1)
	writeOut := func(out []byte) bool {
		if len(out) == 0 {
			return true
		}
		if !started {
			started = true
			att.started = true
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			// 代理 → 客户端响应快照（WriteHeader 前采集；日志四组数据之四）
			att.clientRespStatus = resp.StatusCode
			att.clientRespHeaders = serializeHeaders(w.Header())
			w.WriteHeader(resp.StatusCode)
		}
		wn, werr := w.Write(out)
		if werr != nil {
			att.err = "写入客户端失败: " + werr.Error()
			sentBytes += int64(wn)
			// 失败块的部分写入字节同样计入 clientBytes，与 sentBytes
			// 同口径（sentBytes 含部分写入而 clientBytes 丢弃整块会令两者矛盾）
			clientBytes = append(clientBytes, out[:wn]...)
			lastSent = time.Since(start).Milliseconds()
			return false
		}
		sentBytes += int64(len(out))
		lastSent = time.Since(start).Milliseconds()
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		clientBytes = append(clientBytes, out...)
		return true
	}

	chunk := make([]byte, 64*1024)
	for {
		n, rerr := resp.Body.Read(chunk)
		if n > 0 {
			now := time.Since(start).Milliseconds()
			if firstByte < 0 {
				firstByte = now
			}
			lastChunk = now
			readChunks++
			if int64(len(data)) < maxUpstreamResponseBytes {
				take := n
				if remain := maxUpstreamResponseBytes - int64(len(data)); int64(take) > remain {
					take = int(remain)
				}
				data = append(data, chunk[:take]...)
			}
			out, cerr := conv.FeedChunk(chunk[:n])
			if firstContent < 0 && conv.FirstContentEmitted() {
				firstContent = now
			}
			if !writeOut(out) {
				break
			}
			if cerr != nil {
				if conv.Finished() {
					// 终止帧已送达：响应已完整结束，终止帧之后上游再报错不影响本次调用结果
					break
				}
				// 上游在 200 流中发送错误事件：未开始写客户端时可重试，已开始只能断流
				att.err = cerr.Error()
				break
			}
		}
		if rerr == io.EOF {
			if readChunks == 0 {
				// 2xx 但零字节（空流）：视为失败，可重试
				att.err = "上游流式响应为空"
				break
			}
			if !bytes.Contains(data, []byte("data:")) {
				// 兜底：部分兼容网关会忽略 stream 参数，对 stream=true 仍返回完整 JSON。
				// 按非流式响应解析并一次性合成客户端 SSE（等价旧的“上游强制非流式”路径）。
				if out, ok := fallbackFullBodySSE(pType, clientFormat, data); ok {
					writeOut(out)
					break
				}
				// 上游返回 200 但响应体无法解析（如完整错误 JSON）——按失败处理（可重试），
				// 不应落入 Finish() 伪造"成功"空流掩盖上游错误。此时必然尚未向客户端写出任何
				// 字节（无 data: 帧 → 转换器从未产生输出），可安全换 Key/换供应商重试。
				att.err = "上游返回了无法解析的 200 响应: " + truncate(string(data), 300)
				break
			}
			// 响应体含 data: 帧但全部无法解析出任何有效事件
			//（如 data: <html> 错误页被当作 SSE 帧）——按失败处理（可重试），不应伪造成功空流。
			// 无任何事件 ⇒ 转换器从未产生输出 ⇒ 必然尚未向客户端写出任何字节
			//（与上方口径一致，扩展到「有 data: 前缀但载荷全坏」场景）。
			if !started && !conv.HasAnyEvent() {
				att.err = "上游返回了无法解析的 200 响应: " + truncate(string(data), 300)
				break
			}
			// 上游流正常结束：输出收尾帧（含 usage；幂等）。
			// 残余帧中的错误事件上抛——上游在流末尾发出错误事件且该帧
			// 恰无尾随空行（EOF 截断常见）时，不应静默丢弃错误并伪造成功收尾流。
			// started 语义与 FeedChunk 错误路径一致：已开始转发则断流记失败日志，
			// 未开始（att.started=false）则可安全重试。
			if fout, ferr := conv.Finish(); ferr != nil {
				writeOut(fout)
				att.err = ferr.Error()
			} else {
				writeOut(fout)
			}
			break
		}
		if rerr != nil {
			if conv.Finished() {
				// 终止帧已成功写出：响应已完整送达。客户端（SSE）读完终止事件后即关闭
				// 连接属正常行为，此时若还在等上游剩余字节（[DONE] 后 keep-alive 等），
				// 随后的读错误不应记为"客户端断开"失败——本次调用按成功收尾。
				break
			}
			att.err = "读取上游响应失败: " + rerr.Error()
			break
		}
	}
	if n := conv.DroppedBytes(); n > 0 {
		fmt.Printf("[proxy] 上游流帧缓冲溢出（16MB 上限），丢弃 %d 字节，部分事件不可恢复\n", n)
	}
	att.result.RespBody = data
	att.result.DurationMs = time.Since(start).Milliseconds()
	if firstByte >= 0 {
		att.result.FirstByteMs = firstByte
	}
	att.result.FirstContentMs = firstContent
	if lastChunk >= 0 {
		att.result.LastChunkMs = lastChunk
	} else {
		att.result.LastChunkMs = att.result.DurationMs
	}
	att.result.ReadChunks = readChunks
	att.result.Err = att.err
	att.clientBytes = clientBytes
	att.sentBytes = sentBytes
	att.lastSentMs = lastSent
	att.firstContentMs = firstContent
}

// fallbackFullBodySSE 兜底转换：上游对 stream=true 返回了完整 JSON（忽略 stream 参数的兼容网关）
// 时，按非流式响应解析为 IR 并一次性合成客户端格式的 SSE 事件流。解析失败返回 false。
func fallbackFullBodySSE(pType, clientFormat string, body []byte) ([]byte, bool) {
	ir, err := convert.ParseResponse(pType, body)
	if err != nil {
		return nil, false
	}
	clientResp := convert.BuildResponse(clientFormat, ir, ir.Model)
	sse, err := convert.BuildSSE(clientFormat, clientResp)
	if err != nil {
		return nil, false
	}
	return sse, true
}

// extractStreamUsage 从上游响应体提取 usage（流式路径）。
// 先按 SSE 模式提取；全 0 时回退按整体 JSON 提取——覆盖伪流式兜底场景
// （上游对 stream=true 一次性返回完整 JSON，此时响应体不是 SSE 帧序列，
// 若固定按 SSE 模式提取该路径 token 全记 0）。
// 真 SSE 流无 usage 时两种模式均返回 0，回退无害。
func extractStreamUsage(data []byte) (int64, int64, int64) {
	p, c, t := convert.ExtractUsage(data, true)
	if p == 0 && c == 0 && t == 0 && len(data) > 0 {
		p, c, t = convert.ExtractUsage(data, false)
	}
	return p, c, t
}

// shouldRetry 判断上游错误是否触发重试：400 除外（请求本身有误，换 Key/换供应商重试无意义）。
// 网络错误/超时（statusCode 为 0）、401/403/429/5xx 及其他错误均触发重试。
// 注意：判断必须先看 400 再看 errStr——流式路径的非 2xx 会同时携带状态码与错误串，
// 若先判 errStr 会让 400 误入重试（违反需求 6.4/6.6：上游 400 不重试、归一化 502）。
func shouldRetry(statusCode int) bool {
	if statusCode == http.StatusBadRequest {
		return false
	}
	return true
}

// ===== POST /v1/chat/completions | /v1/responses | /v1/messages =====

// handleProxy 统一代理转发入口；clientFormat 为客户端请求格式（三种之一）。
//
// 转发规则：
//   - stream=true（流式）：边收边转发——同格式上游 SSE 原样实时透传；
//     跨格式经 StreamConverter 把上游事件流实时转换为目标客户端格式输出；
//     首个有效数据前失败可重试，开始转发后失败只能断流（记失败日志）；
//   - stream=false（非流式）：同格式字节级透传；跨格式经 IR 转换后返回客户端格式 JSON。
func (h *Handler) handleProxy(w http.ResponseWriter, r *http.Request, clientFormat string) {
	snap := h.State.Snap()

	// [1] 认证基础项（Key 存在/启用/有效期/IP 白名单）——先于请求体解析（需求 6.3 顺序），
	// 无效 Key 的请求直接 401，不消耗请求体读取与解析
	pk, authErr := h.authenticate(w, r, clientFormat)
	if authErr != nil {
		return
	}

	rawBody, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		convert.WriteError(w, clientFormat, http.StatusBadRequest, "invalid_request_error", "invalid_body", "failed to read request body")
		return
	}
	// [1.5] 按客户端格式解析请求体（宽松解析；格式不合法直接 400）
	ir, perr := convert.ParseRequest(clientFormat, rawBody)
	if perr != nil || ir.Model == "" {
		convert.WriteError(w, clientFormat, http.StatusBadRequest, "invalid_request_error", "invalid_model",
			"you must provide a model parameter")
		return
	}

	// [2] 模型解析（纯读；候选供应商各自携带命中的真实模型名）
	res := h.State.ResolveModel(ir.Model)

	// 请求头快照 + 日志上下文（贯穿各写日志函数；四组数据的客户端请求侧）。
	// 提前到白名单校验之前构建——403 越权拒绝与 404 模型未匹配
	// 同为需审计的安全相关拒绝事件，配置模型白名单的部署中 Key 被滥用探测时
	// 可在调用日志页发现，否则完全无痕。
	reqHeaders := map[string]string{}
	for k, v := range r.Header {
		reqHeaders[k] = strings.Join(v, ", ")
	}
	reqHeadersJSON, _ := json.Marshal(reqHeaders)
	logCtx := &logContext{
		pk: pk, requestedModel: ir.Model, clientReqURL: r.URL.Path,
		reqHeadersJSON: reqHeadersJSON, rawBody: rawBody,
	}

	// [2.5] 模型白名单校验：原始名命中放行全部候选；
	// 否则仅保留自身命中的真实模型名在白名单内的候选（多供应商共享别名时不再整单误拒，
	// 且路由只在白名单允许的候选中选择）；过滤后为空 → 403。
	if len(pk.Models) > 0 {
		filtered := filterCandidatesByWhitelist(res.Candidates, ir.Model, pk.Models)
		if len(filtered) == 0 {
			body := convert.WriteError(w, clientFormat, http.StatusForbidden, "access_denied", "model_not_allowed",
				"model not allowed for this api key")
			// 越权调用记录失败日志（与 404 模型未匹配同口径）
			logCtx.realModel = ""
			h.writeFailureLog(logCtx, nil, nil, nil,
				&clientRespInfo{status: http.StatusForbidden, headers: serializeHeaders(w.Header()), body: body},
				"model not allowed for this api key", nil)
			return
		}
		res.Candidates = filtered
	}

	if res.RealModel == "" || len(res.Candidates) == 0 {
		// 模型未匹配：记录失败日志（错误原因：未匹配到对应模型）后返回 404
		clientResp := writeClientError(w, clientFormat, http.StatusNotFound, "invalid_request_error", "model_not_found",
			fmt.Sprintf("the model '%s' does not exist", ir.Model))
		h.writeModelNotFoundLog(logCtx, clientResp)
		return
	}
	requestedModel := ir.Model

	// [3] 冷却池过滤（按各候选自身命中的模型名）
	candidates := h.State.FilterCooldown(res.Candidates)

	// [4][5][6] 路由 + 重试 + 降级（超时通过 context 控制，支持热加载）
	timeout := snap.UpstreamTimeoutSeconds
	if timeout <= 0 {
		timeout = 300
	}
	// 上游 ctx 脱离 r.Context()（与流式路径同口径）——客户端断开时上游请求
	// 不被连带取消，避免烧光全部重试槽位并把"客户端断开"误记为"全部供应商失败"；
	// 断开检测改由成功后检查 r.Context() 归类（见 [8]）。
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	var attempts []logsdb.AttemptLog
	tried := map[string]bool{}     // 已尝试穷尽的供应商
	var lastResult *upstreamResult // 最后一次尝试（无论成败，供失败日志用）
	var lastCand *core.ProviderCandidate
	var successResult *upstreamResult // 成功结果（nil 表示全部失败）
	var successCand *core.ProviderCandidate
	var successSameFmt bool           // 成功供应商与客户端格式是否相同
	var successIR *convert.ResponseIR // 跨格式成功时的上游响应 IR
	var lastErrMsg string

outer:
	for {
		// 候选 = 模型候选 - 已穷尽（冷却池已在前面过滤，穷尽后新入冷却的通过 tried 排除）
		pool := make([]core.ProviderCandidate, 0, len(candidates))
		for _, c := range candidates {
			if !tried[c.Provider.ID] {
				pool = append(pool, c)
			}
		}
		if len(pool) == 0 {
			break
		}
		cand := h.State.PickProvider(pool, snap.Strategy, res.RealModel)
		if cand == nil {
			break
		}
		p := cand.Provider
		realModel := cand.Model // 该供应商自身命中的真实模型名（别名在不同供应商可指向不同真实名）
		pType := convert.NormalizeAPIType(p.APIType)
		sameFmt := pType == clientFormat
		// 构建上游请求体
		var upBody []byte
		if sameFmt {
			// 同格式：别名/大小写命中时仅替换 model 字段，其余字节级保留；stream 按客户端请求
			upBody = buildUpstreamBody(rawBody, requestedModel, realModel)
		} else {
			// 跨格式：转换为供应商格式；流式随客户端（stream=true 时上游也流式，边收边转换转发）
			up := *ir
			up.Stream = ir.Stream
			b, berr := convert.BuildRequest(pType, &up, realModel)
			if berr != nil {
				errMsg := "构建上游请求失败: " + berr.Error()
				attempts = append(attempts, logsdb.AttemptLog{Provider: p.Name, ErrorMsg: errMsg})
				lastErrMsg = errMsg
				// 补记上游信息（无真实上游请求，构造最小快照供 503 日志使用）
				lastResult = &upstreamResult{Err: errMsg}
				lastCand = cand
				tried[p.ID] = true
				// 构建失败与 Key 无关（同供应商换 Key 重建同样
				// 失败），不应烧 Key 重试槽位。该处应 continue 跳过该供应商进入下一家，
				// 而非 break 直接结束供应商循环（其他可用供应商不再尝试）。
				continue
			}
			upBody = b
		}
		url := convert.UpstreamURL(pType, p.BaseURL)
		// 供应商内 Key 随机不重复尝试，最多 3 次上游请求
		keyOrder := shuffledKeys(p)
		maxAttempts := len(keyOrder)
		if maxAttempts > 3 {
			maxAttempts = 3
		}
		attempted := 0 // 本轮实际发起的上游调用次数（Key>3 供应商冷却入池判定用）
		for i := 0; i < maxAttempts; i++ {
			k := keyOrder[i]
			attempted++
			if ir.Stream {
				// 流式路径：边收边转发（同格式透传 / 跨格式实时转换）
				//
				// 上游 ctx 必须脱离 r.Context() 派生：
				// r.Context() 在客户端断开时被 Go 标准库自动 cancel，会顺 ctx 树取消上游请求，
				// 导致 resp.Body.Read 返回 "context canceled" 被误记为上游失败。
				// 实际场景：客户端断开应被代理正确检测（已通过 w.Write 错误路径 + watchdog goroutine 主动关 resp.Body 处理），
				// 上游继续读完能保留 usage 提取与日志 resp_body 完整性。
				// 仅受 upstream_timeout_seconds 约束。
				streamCtx, streamCancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
				att := h.tryStreamForward(w, streamCtx, r.Context(), p, k.Key, url, upBody, pType, clientFormat, sameFmt, realModel)
				streamCancel()
				if att.err != "" && !att.started {
					// 首个有效数据前失败（网络错误/非 2xx/流首事件错误/空流/无法解析的 200 响应）：可安全重试
					attempts = append(attempts, logsdb.AttemptLog{Provider: p.Name, APIKeyID: k.ID, ErrorMsg: att.err})
					lastResult = att.result
					lastCand = cand
					lastErrMsg = att.err
					if att.clientCanceled {
						// 客户端已断开，重试换 Key/换供应商无人接收响应、
						// 只会白烧上游配额——按失败收尾，记「客户端断开」。
						logCtx.realModel = realModel
						h.writeFailureLog(logCtx, attempts, p, att.result, attClientResp(att), "客户端断开", att)
						return
					}
					if !shouldRetry(att.result.StatusCode) {
						// 上游 400：终止重试，归一化 502（日志消息与返回客户端的一致）
						clientResp := writeClientError(w, clientFormat, http.StatusBadGateway, "bad_gateway", "bad_gateway", "upstream bad request")
						logCtx.realModel = realModel
						h.writeFailureLog(logCtx, attempts, p, lastResult, clientResp, "upstream bad request", att)
						return
					}
					continue
				}
				if att.err != "" {
					// 已开始转发后失败（客户端断开/上游中途断流/流中错误事件）：无法重试，记失败日志
					attempts = append(attempts, logsdb.AttemptLog{Provider: p.Name, APIKeyID: k.ID, ErrorMsg: att.err})
					logCtx.realModel = realModel
					h.writeFailureLog(logCtx, attempts, p, att.result, attClientResp(att), "", att)
					return
				}
				// 成功：数据已实时发给客户端，重置失败计数并写成功日志后返回
				attempts = append(attempts, logsdb.AttemptLog{Provider: p.Name, APIKeyID: k.ID})
				h.State.Cooldown.RecordSuccess(p.ID, realModel)
				usageP, usageC, usageT := extractStreamUsage(att.result.RespBody) // 伪流式兜底时回退 JSON 提取
				logCtx.realModel = realModel
				h.writeSuccessLog(logCtx, attempts, p, att.result, attClientResp(att), usageP, usageC, usageT, true, att)
				return
			}
			result := callUpstream(ctx, h.client, p, k.Key, url, upBody, pType, snap.DefaultUserAgent)
			if result.Err == "" && result.StatusCode >= 200 && result.StatusCode < 300 {
				if sameFmt {
					// 同格式：成功，直接透传
					attempts = append(attempts, logsdb.AttemptLog{Provider: p.Name, APIKeyID: k.ID})
					successResult = result
					successCand = cand
					successSameFmt = true
					break outer
				}
				// 跨格式：解析上游响应为 IR（失败视为本次尝试失败，继续换 Key 重试）
				irResp, cerr := convert.ParseResponse(pType, result.RespBody)
				if cerr != nil {
					errMsg := "上游响应解析失败: " + cerr.Error()
					attempts = append(attempts, logsdb.AttemptLog{Provider: p.Name, APIKeyID: k.ID, ErrorMsg: errMsg})
					lastResult = result
					lastCand = cand
					lastErrMsg = errMsg
					continue
				}
				attempts = append(attempts, logsdb.AttemptLog{Provider: p.Name, APIKeyID: k.ID})
				successResult = result
				successCand = cand
				successSameFmt = false
				successIR = irResp
				break outer
			}
			// 失败
			errMsg := result.Err
			if errMsg == "" {
				errMsg = fmt.Sprintf("upstream HTTP %d: %s", result.StatusCode, truncate(string(result.RespBody), 500))
			}
			attempts = append(attempts, logsdb.AttemptLog{Provider: p.Name, APIKeyID: k.ID, ErrorMsg: errMsg})
			lastResult = result
			lastCand = cand
			lastErrMsg = errMsg
			if r.Context().Err() != nil {
				// 客户端已断开——上游 ctx 脱离 r.Context() 后断开不再
				// 中止重试循环；继续换 Key/换供应商无人接收响应、只会白烧上游配额
				//（最坏 3 Key × N 供应商 × 每次最长超时秒数的上游请求）。
				// 与流式客户端断开、非流式成功后归类同口径终止并记"客户端断开"，
				// 上游失败详情保留在 attempt 记录中可审计。
				logCtx.realModel = realModel
				h.writeFailureLog(logCtx, attempts, p, lastResult, nil, "客户端断开", nil)
				return
			}
			if !shouldRetry(result.StatusCode) {
				// 上游 400：终止重试，归一化 502（日志消息与返回客户端的一致）。
				// 到达此处时必然尚未成功（成功即 break outer），无需再判 successCand。
				clientResp := writeClientError(w, clientFormat, http.StatusBadGateway, "bad_gateway", "bad_gateway", "upstream bad request")
				logCtx.realModel = realModel
				h.writeFailureLog(logCtx, attempts, p, lastResult, clientResp, "upstream bad request", nil)
				return
			}
		}
		// Key 数 >3 的供应商应按"按上限实际尝试且全部失败"即计一轮失败——
		// 3 次浅尝全败与试完全部 Key 全败同为该供应商该模型的失败信号
		//（RecordFailure 内部仍有连续轮数阈值把关）。
		// 注意：构建失败 continue 时不经过此处，不再计失败轮——构建失败属
		// 跨格式转换层问题、与供应商 Key 健康无关，计入冷却反而让用户困惑。
		if attempted > 0 && attempted == maxAttempts && snap.CooldownEnabled {
			// 冷却池已启用：累计一轮“全部 Key 失败”；连续达到重试次数阈值后才入池
			h.State.Cooldown.RecordFailure(p.ID, realModel, snap.CooldownSeconds, snap.CooldownRetryCount)
		}
		tried[p.ID] = true
	}

	// [7] 所有供应商均失败 →503
	if successResult == nil || successCand == nil {
		msg := "all providers failed"
		if lastErrMsg != "" {
			msg = "all providers failed: " + truncate(lastErrMsg, 300)
		}
		if len(res.Candidates) > 0 && len(candidates) == 0 {
			// 全部候选都在冷却中
			msg = fmt.Sprintf("the model '%s' is temporarily unavailable (all providers cooling down)", res.RealModel)
		} else if len(attempts) == 0 {
			// 有候选但一次上游请求都未发出（如选中供应商无可用 Key）
			msg += " (no provider attempted)"
		}
		// 日志与返回给客户端的消息保持一致——cooling down 场景下不应误记为
		// "all providers failed (no provider attempted)"。
		clientResp := writeClientError(w, clientFormat, http.StatusServiceUnavailable, "service_unavailable", "service_unavailable", msg)
		if lastCand != nil {
			logCtx.realModel = lastCand.Model
		}
		h.writeFailureLog(logCtx, attempts, providerOf(lastCand), lastResult, clientResp, msg, nil)
		return
	}

	// [8] 成功响应处理（非流式路径；流式已在上面实时转发并写日志返回）
	// 成功即重置该供应商该模型的连续失败计数（冷却池重试阈值语义）
	h.State.Cooldown.RecordSuccess(successCand.Provider.ID, successCand.Model)
	logCtx.realModel = successCand.Model
	// 客户端已断开时（r.Context() 已取消），上游响应虽完整但未送达——
	// 与流式路径同口径记 failure"客户端断开"，不应把断开误归类为上游失败烧重试槽位
	if r.Context().Err() != nil {
		h.writeFailureLog(logCtx, attempts, successCand.Provider, successResult, nil, "客户端断开", nil)
		return
	}
	var clientResp *clientRespInfo
	var usageP, usageC, usageT int64
	if successSameFmt {
		// 同格式：响应原样转发
		usageP, usageC, usageT = convert.ExtractUsage(successResult.RespBody, false)
		copyUpstreamHeaders(w, successResult.RespHeaders)
		w.Header().Set("Content-Length", fmt.Sprint(len(successResult.RespBody)))
		// 代理 → 客户端响应快照（WriteHeader 前采集；日志四组数据之四）
		clientResp = &clientRespInfo{status: successResult.StatusCode,
			headers: serializeHeaders(w.Header()), body: successResult.RespBody}
		w.WriteHeader(successResult.StatusCode)
		_, _ = w.Write(successResult.RespBody)
	} else {
		// 跨格式：转换回客户端格式（非流式 JSON）
		clientJSON := convert.BuildResponse(clientFormat, successIR, successIR.Model)
		usageP, usageC, usageT = successIR.PromptTokens, successIR.CompletionTokens, successIR.TotalTokens
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Length", fmt.Sprint(len(clientJSON)))
		// 代理 → 客户端响应快照（WriteHeader 前采集；日志四组数据之四）
		clientResp = &clientRespInfo{status: successResult.StatusCode,
			headers: serializeHeaders(w.Header()), body: clientJSON}
		w.WriteHeader(successResult.StatusCode)
		_, _ = w.Write(clientJSON)
	}

	// [9] 日志写入（成功，最终只写 1 条）；非流式路径 streamed=false
	h.writeSuccessLog(logCtx, attempts, successCand.Provider, successResult, clientResp, usageP, usageC, usageT, false, nil)
}

// providerOf 安全取出候选的供应商（nil 安全）。
func providerOf(c *core.ProviderCandidate) *config.Provider {
	if c == nil {
		return nil
	}
	return c.Provider
}

// ===== 日志写入 =====

// retryCountFrom 由尝试记录推导重试次数（总尝试数-1；0 次尝试记 0）。
// 成功场景：最后一条为成功，len-1 = 此前失败次数；失败场景：len-1 = 重试次数。
func retryCountFrom(attempts []logsdb.AttemptLog) int {
	if len(attempts) == 0 {
		return 0
	}
	return len(attempts) - 1
}

// generationMs 计算生成耗时（token 速率的分母）。
//
// 口径：
//   - 流式：从上游（模型供应商）首个响应字节到达起算，到末字节结束——
//     即"模型实际吐字窗口"，不含代理排队/上游排队/网络往返；
//   - 非流式：整个上游请求的耗时（请求发出 → 响应体读完毕）；
//   - 流式异常（未收到首字节）回退总耗时；result 为 nil 返回 0。
func generationMs(result *upstreamResult, streamed bool) int64 {
	if result == nil {
		return 0
	}
	if streamed && result.FirstByteMs >= 0 && result.LastChunkMs > result.FirstByteMs {
		return result.LastChunkMs - result.FirstByteMs
	}
	return result.DurationMs
}

// outputTokensPerSecond 计算输出 token 速率（completion_tokens ÷ 生成耗时秒）；耗时或 token 为 0 时记 0。
func outputTokensPerSecond(completionTokens, generationMs int64) float64 {
	if completionTokens <= 0 || generationMs <= 0 {
		return 0
	}
	return float64(completionTokens) / (float64(generationMs) / 1000.0)
}

// logContext 一次代理调用的日志上下文：贯穿各写日志函数，
// 携带四组数据中"客户端 → 代理"侧的原始信息。
type logContext struct {
	pk             *config.ProxyKey
	requestedModel string
	realModel      string
	clientReqURL   string // 客户端请求路径（如 /v1/responses）
	reqHeadersJSON []byte // 客户端请求头（JSON）
	rawBody        []byte // 客户端请求体
}

// clientRespInfo 代理 → 客户端响应快照（日志四组数据之四）。
type clientRespInfo struct {
	status  int    // 写给客户端的 HTTP 状态码
	headers string // 写给客户端的响应头（JSON，WriteHeader 前采集）
	body    []byte // 写给客户端的响应体（流式为已发送部分）
}

// attClientResp 从流式尝试结果构造客户端响应快照。
func attClientResp(att *streamAttempt) *clientRespInfo {
	if att == nil {
		return nil
	}
	return &clientRespInfo{status: att.clientRespStatus, headers: att.clientRespHeaders, body: att.clientBytes}
}

// writeClientError 按客户端格式写错误响应，并返回客户端响应快照（供日志记录）。
func writeClientError(w http.ResponseWriter, clientFormat string, status int, errType, code, message string) *clientRespInfo {
	body := convert.WriteError(w, clientFormat, status, errType, code, message)
	return &clientRespInfo{status: status, headers: serializeHeaders(w.Header()), body: body}
}

// logEnabled 日志记录开关是否开启：关闭时代理不写任何调用日志。
// 每次写日志前从快照实时读取，支持热加载（设置页保存 / 手改 config.yml 后立即生效）。
func (h *Handler) logEnabled() bool { return h.State.Snap().LogEnabled }

func (h *Handler) writeSuccessLog(ctx *logContext, attempts []logsdb.AttemptLog, p *config.Provider,
	result *upstreamResult, clientResp *clientRespInfo,
	usageP, usageC, usageT int64, upstreamStreamed bool,
	streamMetrics *streamAttempt) {

	if !h.logEnabled() {
		return
	}
	entry := h.buildLog(ctx, attempts, p, result)
	entry.Status = "success"
	entry.PromptTokens = usageP
	entry.CompletionTokens = usageC
	entry.TotalTokens = usageT
	entry.GenerationMs = generationMs(result, upstreamStreamed)
	entry.TokensPerSecond = outputTokensPerSecond(usageC, entry.GenerationMs)
	fillClientResp(entry, clientResp)
	if streamMetrics != nil {
		entry.SentBytes = streamMetrics.sentBytes
		entry.LastSentMs = streamMetrics.lastSentMs
		entry.FirstContentMs = streamMetrics.firstContentMs
	}
	if store := h.LogStore(); store != nil {
		store.WriteAsync(entry)
	}
}

func (h *Handler) writeFailureLog(ctx *logContext, attempts []logsdb.AttemptLog, p *config.Provider,
	result *upstreamResult, clientResp *clientRespInfo, clientMsg string,
	streamMetrics *streamAttempt) {

	if !h.logEnabled() {
		return
	}
	entry := h.buildLog(ctx, attempts, p, result)
	entry.Status = "failure"
	msg := "all providers failed"
	if entry.ErrorMsg != nil {
		msg = *entry.ErrorMsg
	}
	if clientMsg != "" {
		// 调用方给出的最终消息（与返回给客户端的一致）：优先采用，
		// 保证日志与客户端可见信息一致（如 all providers cooling down）。
		msg = clientMsg
	} else if p == nil {
		msg = msg + " (no provider attempted)"
	}
	entry.ErrorMsg = &msg
	fillClientResp(entry, clientResp)
	if streamMetrics != nil {
		entry.SentBytes = streamMetrics.sentBytes
		entry.LastSentMs = streamMetrics.lastSentMs
		entry.FirstContentMs = streamMetrics.firstContentMs
	}
	if store := h.LogStore(); store != nil {
		store.WriteAsync(entry)
	}
}

// fillClientResp 填充代理 → 客户端响应三元组（日志四组数据之四）。
func fillClientResp(entry *logsdb.CallLog, clientResp *clientRespInfo) {
	if clientResp == nil {
		return
	}
	entry.ClientRespStatus = clientResp.status
	entry.ClientRespHeaders = clientResp.headers
	entry.ClientRespBody = truncate(string(clientResp.body), 1<<20)
}

// writeModelNotFoundLog 记录模型未匹配的失败日志（认证已通过、未尝试任何供应商、无上游响应）。
func (h *Handler) writeModelNotFoundLog(ctx *logContext, clientResp *clientRespInfo) {
	if !h.logEnabled() {
		return
	}
	entry := h.buildLog(ctx, nil, nil, nil)
	entry.Status = "failure"
	msg := "未匹配到对应模型"
	entry.ErrorMsg = &msg
	fillClientResp(entry, clientResp)
	if store := h.LogStore(); store != nil {
		store.WriteAsync(entry)
	}
}

// buildLog 构造日志主体：四组数据中"客户端请求"（ctx）、"上游请求/响应"（result）两组在此填充；
// "客户端响应"由 fillClientResp 补充。
func (h *Handler) buildLog(ctx *logContext, attempts []logsdb.AttemptLog, p *config.Provider,
	result *upstreamResult) *logsdb.CallLog {

	entry := &logsdb.CallLog{
		ID:             core.NewID(),
		Timestamp:      logsdb.NowRFC3339(),
		APIKey:         ctx.pk.Key,
		APIKeyID:       ctx.pk.ID,
		APIKeyName:     ctx.pk.Name,
		RetryCount:     retryCountFrom(attempts),
		AttemptLogs:    attempts,
		RequestedModel: ctx.requestedModel,
		ActualModel:    ctx.realModel,
		ReqMethod:      http.MethodPost,
		ReqBody:        truncate(string(ctx.rawBody), 1<<20), // 与其他三组同口径 1MB 入库截断（需求 4.3）
		ClientReqURL:   ctx.clientReqURL,
	}
	if len(ctx.reqHeadersJSON) > 0 {
		entry.ReqHeaders = string(ctx.reqHeadersJSON)
	}
	if p != nil {
		entry.Provider = p.Name
	}
	if result != nil {
		entry.DurationMs = result.DurationMs
		entry.GenerationMs = result.DurationMs // 默认：非流式/失败场景生成耗时≈总耗时（成功流式场景由 writeSuccessLog 精确覆盖）
		// 代理 → 上游请求（四组数据之二；认证头已在采集时脱敏）
		entry.UpstreamURL = result.ReqURL
		if result.ReqHeaders != nil {
			b, _ := json.Marshal(result.ReqHeaders)
			entry.UpstreamReqHeaders = string(b)
		}
		if result.ReqBody != nil {
			entry.UpstreamReqBody = truncate(string(result.ReqBody), 1<<20)
		}
		// 上游 → 代理响应（四组数据之三）
		entry.RespHeaders = serializeHeaders(result.RespHeaders)
		entry.RespStatus = result.StatusCode
		entry.RespBody = truncate(string(result.RespBody), 1<<20)
		// 流式阶段采集的时刻指标同步到日志（首字节为流式 token 速率起算点）。
		if result.FirstByteMs >= 0 {
			entry.FirstByteMs = result.FirstByteMs
		}
		if result.LastChunkMs > 0 {
			entry.LastChunkMs = result.LastChunkMs
		}
		if result.FirstContentMs > 0 {
			entry.FirstContentMs = result.FirstContentMs
		}
		if result.Err != "" {
			errMsg := truncate(result.Err, 500)
			entry.ErrorMsg = &errMsg
		} else if result.StatusCode >= 400 {
			errMsg := fmt.Sprintf("upstream HTTP %d", result.StatusCode)
			entry.ErrorMsg = &errMsg
		}
	}
	return entry
}

// ===== 工具 =====

func shuffledKeys(p *config.Provider) []config.ProviderAPIKey {
	// 先收集启用的 Key——停用 Key 在路由阶段已被候选过滤，理论上不会到这里，
	// 但 shuffledKeys 也兜底过滤：防御性，避免某供应商部分 Key 停用时随机到 disabled
	// Key 浪费上游配额。返回 nil 时调用方应将整供应商视为无可用 Key（同 0 Key 处理）。
	enabled := make([]config.ProviderAPIKey, 0, len(p.APIKeys))
	for _, k := range p.APIKeys {
		if k.Enabled {
			enabled = append(enabled, k)
		}
	}
	n := len(enabled)
	if n == 0 {
		return nil
	}
	out := make([]config.ProviderAPIKey, n)
	copy(out, enabled)
	// Fisher-Yates 洗牌
	for i := n - 1; i > 0; i-- {
		j := randIntn(i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return out
}

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

// truncate 按字节上限截断字符串，附加 "...(truncated)" 标记（前端依赖该标记识别截断）。
// 截断点回退到最近的 UTF-8 字符边界，避免把中文等多字节字符截半产生乱码。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	end := n
	for end > 0 && !utf8.RuneStart(s[end]) {
		end-- // 回退到多字节字符的起始边界（最多退 3 字节）
	}
	return s[:end] + "...(truncated)"
}
