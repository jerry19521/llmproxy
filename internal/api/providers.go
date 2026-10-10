package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"llmproxy/internal/config"
	"llmproxy/internal/convert"
	"llmproxy/internal/core"
)

// FlexAliases 兼容字符串（逗号分割）与字符串数组两种别名输入。
type FlexAliases []string

// UnmarshalJSON 兼容 "a,b" 与 ["a","b"]。
// v1.8.3（L7）：非法类型（数字/对象/布尔）不再吞错归为空数组，而是报错——
// 让请求体解码阶段直接 400，配置错误不再被掩盖为“无别名”。
func (a *FlexAliases) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*a = splitAliases(s)
		return nil
	}
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		*a = cleanAliases(arr)
		return nil
	}
	return fmt.Errorf("aliases 需为字符串（英文逗号分割）或字符串数组")
}

// splitAliases 按英文逗号分割并去空格。
func splitAliases(s string) []string {
	parts := strings.Split(s, ",")
	out := []string{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func cleanAliases(in []string) []string {
	out := []string{}
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// providerPayload 供应商表单。
type providerPayload struct {
	Name    string  `json:"name"`
	BaseURL string  `json:"base_url"`
	APIType *string `json:"api_type"` // openai(默认) | responses | anthropic；nil=保持原值（新增时默认 openai）
	Weight  *int    `json:"weight"`
	Enabled *bool   `json:"enabled"`
	APIKeys []struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	} `json:"api_keys"`
	Headers []config.ProviderHeader `json:"headers"`
	Models  []struct {
		Name    string      `json:"name"`
		Aliases FlexAliases `json:"aliases"`
	} `json:"models"`
}

// providerView 供应商视图（Key 脱敏）。
type providerView struct {
	ID        string                  `json:"id"`
	Name      string                  `json:"name"`
	BaseURL   string                  `json:"base_url"`
	APIType   string                  `json:"api_type"`
	Enabled   bool                    `json:"enabled"`
	Weight    int                     `json:"weight"`
	APIKeys   []map[string]string     `json:"api_keys"`
	Headers   []config.ProviderHeader `json:"headers"`
	Models    []config.ProviderModel  `json:"models"`
	CreatedAt string                  `json:"created_at"`
	UpdatedAt string                  `json:"updated_at"`
}

func maskKeys(keys []config.ProviderAPIKey) []map[string]string {
	out := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]string{"id": k.ID, "key": core.MaskKey(k.Key)})
	}
	return out
}

func toView(p *config.Provider) *providerView {
	w := p.Weight
	if w <= 0 {
		w = 1
	}
	return &providerView{
		ID: p.ID, Name: p.Name, BaseURL: p.BaseURL, APIType: convert.NormalizeAPIType(p.APIType),
		Enabled: p.Enabled, Weight: w,
		APIKeys: maskKeys(p.APIKeys), Headers: p.Headers, Models: p.Models,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

// validateModelsRaw 对原始模型列表做同供应商内唯一性校验（大小写不敏感）。
func validateModelsRaw(models []struct {
	Name    string      `json:"name"`
	Aliases FlexAliases `json:"aliases"`
}) error {
	seen := map[string]bool{}
	for _, m := range models {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			continue
		}
		lk := strings.ToLower(name)
		if seen[lk] {
			return fmt.Errorf("同供应商内模型名称不能重复: %s", name)
		}
		seen[lk] = true
	}
	return nil
}

func (p *providerPayload) toProviderModels() []config.ProviderModel {
	out := []config.ProviderModel{}
	seen := map[string]bool{}
	for _, m := range p.Models {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			continue
		}
		lk := strings.ToLower(name)
		if seen[lk] {
			continue // 新增时重名跳过
		}
		seen[lk] = true
		out = append(out, config.ProviderModel{Name: name, Aliases: cleanAliases(m.Aliases)})
	}
	return out
}

// validateProviderForm 表单静态校验（不依赖内存状态，可在 Mutate 前执行）。
func validateProviderForm(body *providerPayload) error {
	if strings.TrimSpace(body.Name) == "" {
		return fmt.Errorf("名称不能为空")
	}
	base := strings.TrimSuffix(strings.TrimSpace(body.BaseURL), "/")
	if base == "" {
		return fmt.Errorf("基础地址不能为空")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return fmt.Errorf("基础地址需以 http:// 或 https:// 开头")
	}
	if body.APIType != nil && !convert.ValidAPIType(*body.APIType) {
		return fmt.Errorf("接口类型无效：%s（可选 openai / responses / anthropic）", *body.APIType)
	}
	return validateModelsRaw(body.Models)
}

// maskedRestoreByKeyID 判断表单提交的 Key 是否为某现有 Key 的脱敏回显值（未修改）。
// 是则返回完整值。v1.8.4（A9）：以「提交值 == MaskKey(现有 Key)」精确比对取代
// Contains("***") 内容特征——部分上游网关的自定义凭证可能字面含 ***，内容特征会误判：
// 更新场景静默把用户修改的 Key 恢复成旧值、恢复不到 id 时整行静默丢弃，排查困难。
func maskedRestoreByKeyID(submitted, id string, saved []config.ProviderAPIKey) (string, bool) {
	for _, old := range saved {
		if old.ID == id && submitted == core.MaskKey(old.Key) {
			return old.Key, true
		}
	}
	return "", false
}

// applyPayload 将表单数据应用到 Provider（key 脱敏保留逻辑）。调用前需先 validateProviderForm。
func applyPayload(p *config.Provider, body *providerPayload, isCreate bool) {
	name := strings.TrimSpace(body.Name)
	base := strings.TrimSuffix(strings.TrimSpace(body.BaseURL), "/")
	p.Name = name
	p.BaseURL = base
	if body.APIType != nil {
		p.APIType = convert.NormalizeAPIType(*body.APIType)
	} else if isCreate {
		p.APIType = convert.FormatOpenAI
	}
	if body.Weight != nil && *body.Weight > 0 {
		p.Weight = *body.Weight
	} else if isCreate {
		p.Weight = 1
	}
	if body.Enabled != nil {
		p.Enabled = *body.Enabled
	} else if isCreate {
		p.Enabled = true
	}
	// API Keys：脱敏回显值按 id 恢复完整值
	// v1.8.4（A9）：精确比对取代 Contains("***") 内容特征（真实上游 Key 可能字面含 ***）
	oldKeys := p.APIKeys
	keys := []config.ProviderAPIKey{}
	for _, k := range body.APIKeys {
		key := strings.TrimSpace(k.Key)
		if key == "" {
			continue
		}
		if !isCreate {
			if full, restored := maskedRestoreByKeyID(key, k.ID, oldKeys); restored {
				// 未修改的脱敏回显：保留原 Key 原生成 id
				keys = append(keys, config.ProviderAPIKey{ID: k.ID, Key: full})
				continue
			}
		}
		id := k.ID
		// 临时 id（前端新增行 tmp-N / 测试接口 key-N）不写入配置，重新生成 UUID
		if id == "" || strings.HasPrefix(id, "tmp-") || strings.HasPrefix(id, "key-") {
			id = core.NewID()
		}
		keys = append(keys, config.ProviderAPIKey{ID: id, Key: key})
	}
	p.APIKeys = keys
	// Headers
	headers := []config.ProviderHeader{}
	for _, h := range body.Headers {
		if strings.TrimSpace(h.Key) != "" {
			headers = append(headers, config.ProviderHeader{Key: strings.TrimSpace(h.Key), Value: h.Value})
		}
	}
	p.Headers = headers
	p.Models = body.toProviderModels()
}

// ===== 供应商 CRUD =====

func (s *Server) handleProviderList(w http.ResponseWriter, r *http.Request) {
	snap := s.State.Snap()
	out := make([]*providerView, 0, len(snap.Providers))
	for _, p := range snap.Providers {
		out = append(out, toView(p))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"providers": out})
}

func (s *Server) handleProviderGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := s.State.FindProvider(id)
	if p == nil {
		writeErr(w, http.StatusNotFound, "provider not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, toView(p))
}

func (s *Server) handleProviderCreate(w http.ResponseWriter, r *http.Request) {
	var body providerPayload
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body", "invalid_request")
		return
	}
	if err := validateProviderForm(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}
	now := time.Now().Format(time.RFC3339)
	p := &config.Provider{ID: core.NewID(), CreatedAt: now, UpdatedAt: now}
	applyPayload(p, &body, true)
	err := s.State.Mutate(func(ns *core.Snapshot) {
		ns.Providers = append(ns.Providers, p)
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error(), "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, toView(p))
}

func (s *Server) handleProviderUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body providerPayload
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body", "invalid_request")
		return
	}
	if err := validateProviderForm(&body); err != nil {
		code := "invalid_request"
		if strings.Contains(err.Error(), "重复") {
			code = "duplicate_model_name"
		}
		writeErr(w, http.StatusBadRequest, err.Error(), code)
		return
	}
	var updated *config.Provider
	found := false
	err := s.State.Mutate(func(ns *core.Snapshot) {
		for i, p := range ns.Providers {
			if p.ID == id {
				applyPayload(p, &body, false)
				p.UpdatedAt = time.Now().Format(time.RFC3339)
				ns.Providers[i] = p
				updated = p
				found = true
				return
			}
		}
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error(), "internal_error")
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "provider not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, toView(updated))
}

func (s *Server) handleProviderDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	found := false
	err := s.State.Mutate(func(ns *core.Snapshot) {
		out := ns.Providers[:0]
		for _, p := range ns.Providers {
			if p.ID == id {
				found = true
				continue
			}
			out = append(out, p)
		}
		ns.Providers = out
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "删除失败: "+err.Error(), "internal_error")
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "provider not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleProviderEnable(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		found := false
		err := s.State.Mutate(func(ns *core.Snapshot) {
			for _, p := range ns.Providers {
				if p.ID == id {
					p.Enabled = enabled
					p.UpdatedAt = time.Now().Format(time.RFC3339)
					found = true
					return
				}
			}
		})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "操作失败: "+err.Error(), "internal_error")
			return
		}
		if !found {
			writeErr(w, http.StatusNotFound, "provider not found", "not_found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// ===== 供应商 API Key 测试 =====

type testResult struct {
	KeyID     string       `json:"key_id"`
	MaskedKey string       `json:"masked_key"`
	Success   bool         `json:"success"`
	Error     string       `json:"error,omitempty"`
	Request   testReqView  `json:"request"`
	Response  testRespView `json:"response"`
}

type testReqView struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

type testRespView struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// testOneKey 用指定的 base_url、请求头与 Key 对单个 Key 发起测试（结果不含 key_id/masked_key，由调用方填充）。
// 按供应商接口类型（api_type）选择测试请求格式与端点。
// v1.8.4（A10）：max_tokens 从 4096 降到 16——连通性测试目的是验证鉴权与端点可达，
// 4096 允许模型生成最多 4096 输出 token，全部计入上游账单；50 条 Key 串行测试时
// 副作用×50（主流网关对 max_tokens=1 也能通过校验，16 留有裕量）。
// v1.8.4（A2）：接收 ctx——由调用方的整体时间预算控制，超时后剩余 Key 不再发请求。
func (s *Server) testOneKey(ctx context.Context, apiType, baseURL string, headers []config.ProviderHeader, key, model, testMsg string, client *http.Client) testResult {
	apiType = convert.NormalizeAPIType(apiType)
	var reqBody []byte
	switch apiType {
	case convert.FormatResponses:
		reqBody, _ = json.Marshal(map[string]interface{}{
			"model": model, "input": testMsg, "max_output_tokens": 16,
		})
	case convert.FormatAnthropic:
		reqBody, _ = json.Marshal(map[string]interface{}{
			"model": model, "messages": []map[string]string{{"role": "user", "content": testMsg}}, "max_tokens": 16,
		})
	default:
		reqBody, _ = json.Marshal(map[string]interface{}{
			"model": model, "messages": []map[string]string{{"role": "user", "content": testMsg}}, "max_tokens": 16,
		})
	}
	url := convert.UpstreamURL(apiType, baseURL)
	res := testResult{}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		res.Error = "构建请求失败: " + err.Error()
		res.Response = testRespView{Status: 0, Headers: map[string]string{}, Body: ""}
		return res
	}
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	convert.SetAuthHeaders(req.Header, apiType, key)
	// v1.8.6：全局默认 UA 先于供应商自定义头应用（后者可覆盖）；
	// 背景：腾讯元宝 bot 等网关对 Go 默认 UA 返回 403，测试 Key 与真实转发同口径
	convert.SetDefaultUserAgent(req.Header, s.State.Snap().DefaultUserAgent)
	for _, h := range headers {
		if h.Key != "" {
			req.Header.Set(h.Key, h.Value)
		}
	}
	// M6 修复：测试详情回显的认证头脱敏（保留前 10 + 尾 4，与调用日志四组数据同口径），
	// 避免完整上游 Key 经 HTTP 响应明文回显（列表/详情接口均已脱敏，此处不应成为旁路）。
	hdrs := map[string]string{}
	for hk, hv := range req.Header {
		val := strings.Join(hv, ", ")
		switch strings.ToLower(hk) {
		case "authorization", "x-api-key":
			if len(val) > 14 {
				val = val[:10] + "..." + val[len(val)-4:]
			} else {
				val = "***"
			}
		}
		hdrs[hk] = val
	}
	res.Request = testReqView{URL: url, Method: http.MethodPost, Headers: hdrs, Body: string(reqBody)}
	resp, err := client.Do(req)
	if err != nil {
		res.Error = err.Error()
		res.Response = testRespView{Status: 0, Headers: map[string]string{}, Body: ""}
		return res
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	rhdrs := map[string]string{}
	for hk, hv := range resp.Header {
		rhdrs[hk] = strings.Join(hv, ", ")
	}
	res.Response = testRespView{Status: resp.StatusCode, Headers: rhdrs, Body: string(respBody)}
	if resp.StatusCode == 200 {
		// 校验返回格式合法性（按接口类型检查特征字段）
		var obj map[string]json.RawMessage
		if jerr := json.Unmarshal(respBody, &obj); jerr != nil {
			res.Error = "响应不是合法 JSON: " + jerr.Error()
		} else if e, ok := obj["error"]; ok && len(e) > 0 && string(e) != "null" {
			res.Error = "上游返回错误: " + truncateStr(string(respBody), 300)
		} else {
			var signal string
			switch apiType {
			case convert.FormatResponses:
				signal = "output"
			case convert.FormatAnthropic:
				signal = "content"
			default:
				signal = "choices"
			}
			if _, ok := obj[signal]; ok {
				res.Success = true
			} else {
				res.Error = "响应缺少 " + signal + " 字段，可能不是该类型的接口"
			}
		}
	} else {
		res.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return res
}

// handleProviderTest POST /api/providers/test
// 表单模式：直接使用前端当前填写的基础地址、API Key 与请求头测试（无需先保存供应商）。
// 含 *** 的脱敏 Key 按 provider_id + key id 从已保存供应商恢复完整 Key；无法恢复的跳过。
func (s *Server) handleProviderTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProviderID string `json:"provider_id,omitempty"`
		BaseURL    string `json:"base_url"`
		APIType    string `json:"api_type"`
		APIKeys    []struct {
			ID  string `json:"id"`
			Key string `json:"key"`
		} `json:"api_keys"`
		Headers []config.ProviderHeader `json:"headers"`
		Model   string                  `json:"model"`
		KeyIDs  []string                `json:"key_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body", "invalid_request")
		return
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		writeErr(w, http.StatusBadRequest, "测试模型不能为空，请先在模型列表中选择", "invalid_request")
		return
	}
	base := strings.TrimSuffix(strings.TrimSpace(body.BaseURL), "/")
	if base == "" {
		writeErr(w, http.StatusBadRequest, "基础地址不能为空", "invalid_request")
		return
	}
	// v1.8.4（A11）：与 CRUD 表单校验同口径——非法前缀延迟到 client.Do 才以
	// 原始传输层错误失败，提示不友好且两套口径不一致
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		writeErr(w, http.StatusBadRequest, "基础地址需以 http:// 或 https:// 开头", "invalid_request")
		return
	}
	// 已保存供应商（用于恢复脱敏 Key）
	var saved *config.Provider
	if body.ProviderID != "" {
		saved = s.State.FindProvider(body.ProviderID)
	}
	// 组装待测 Key：脱敏值恢复、空值跳过、key_ids 过滤
	type keyTarget struct {
		id, key, masked string
	}
	kidSet := map[string]bool{}
	for _, kid := range body.KeyIDs {
		kidSet[kid] = true
	}
	targets := []keyTarget{}
	for i, k := range body.APIKeys {
		if len(kidSet) > 0 && !kidSet[k.ID] {
			continue
		}
		key := strings.TrimSpace(k.Key)
		if key == "" {
			continue
		}
		id := k.ID
		if id == "" {
			id = fmt.Sprintf("key-%d", i)
		}
		// v1.8.4（A9）：提交值恰为现有 Key 的脱敏值时视为未修改，恢复完整值；
		// 否则按新输入处理（真实 Key 含 *** 不再被误恢复/误跳过）
		if saved != nil {
			if full, ok := maskedRestoreByKeyID(key, k.ID, saved.APIKeys); ok {
				targets = append(targets, keyTarget{id: k.ID, key: full, masked: core.MaskKey(full)})
				continue
			}
		}
		targets = append(targets, keyTarget{id: id, key: key, masked: core.MaskKey(key)})
	}
	if len(targets) == 0 {
		writeErr(w, http.StatusBadRequest, "没有可测试的 API Key（请检查 Key 是否已填写，或脱敏 Key 是否对应已保存的供应商）", "invalid_request")
		return
	}
	testMsg := s.State.Snap().TestMessage
	if testMsg == "" {
		testMsg = "hi"
	}
	// v1.8.3（L12）：单次测试 Key 数量上限——测试串行执行且每 Key 超时 120s，
	// 不设上限时 8MB 请求体内可塞入大量 Key 目标把请求挂数分钟。
	const maxTestTargets = 50
	if len(targets) > maxTestTargets {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("单次最多测试 %d 条 API Key", maxTestTargets), "invalid_request")
		return
	}
	client := &http.Client{Timeout: 120 * time.Second}
	results := make([]testResult, 0, len(targets))
	// v1.8.4（A2）：整体时间预算——上限 50 × 单 Key 120s 的最坏组合约 100 分钟，
	// 远超 L12 注释自述的「挂数分钟」目标；以 r.Context() 派生 10 分钟总预算，
	// 超预算后剩余 Key 直接标记跳过（客户端断开同样会提前终止进行中的请求）。
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	for _, t := range targets {
		if ctx.Err() != nil {
			results = append(results, testResult{KeyID: t.id, MaskedKey: t.masked, Error: "总超时（10 分钟预算耗尽），已跳过"})
			continue
		}
		res := s.testOneKey(ctx, body.APIType, base, body.Headers, t.key, model, testMsg, client)
		res.KeyID = t.id
		res.MaskedKey = t.masked
		results = append(results, res)
	}
	// 按请求顺序输出：保持 targets 顺序即可
	writeJSON(w, http.StatusOK, map[string]interface{}{"results": results})
}

// ===== 自动获取模型 =====

// parseModelsResponse 兼容常见 /models 响应格式。
func parseModelsResponse(body []byte) []string {
	var top interface{}
	if err := json.Unmarshal(body, &top); err != nil {
		return nil
	}
	var arr []interface{}
	switch v := top.(type) {
	case []interface{}:
		arr = v
	case map[string]interface{}:
		if d, ok := v["data"].([]interface{}); ok {
			arr = d
		} else if d, ok := v["models"].([]interface{}); ok {
			arr = d
		}
		// v1.8.2（C2）：移除无效的 `v["data"].(map)` 死分支（原实现仅 `_ = d`，无任何效果）
	}
	seen := map[string]bool{}
	out := []string{}
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		id := ""
		if v, ok := m["id"].(string); ok && v != "" {
			id = v
		} else if v, ok := m["name"].(string); ok && v != "" {
			id = v
		} else if v, ok := m["model"].(string); ok && v != "" {
			id = v
		}
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// v1.8.5（G5，关联检查报告 B1）：接收 ctx——调用方可施加整体预算、随客户端
// 断开取消；原实现 NewRequest 不带 ctx，仅靠 per-request 超时兜底，串行多 Key
// 尝试时整体时长无上界。
func fetchUpstreamModels(ctx context.Context, apiType, baseURL string, headers []config.ProviderHeader, key, defaultUA string, timeout time.Duration) ([]string, error) {
	url := convert.ModelsURL(apiType, baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	convert.SetAuthHeaders(req.Header, apiType, key)
	// v1.8.6：全局默认 UA（供应商自定义头可覆盖），与 testOneKey/newUpstreamRequest 同口径
	convert.SetDefaultUserAgent(req.Header, defaultUA)
	for _, h := range headers {
		if h.Key != "" {
			req.Header.Set(h.Key, h.Value)
		}
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 300))
	}
	models := parseModelsResponse(body)
	if len(models) == 0 {
		return nil, fmt.Errorf("响应中未解析到模型列表")
	}
	return models, nil
}

// handleFetchModelsByID GET /api/providers/fetch-models?provider_id=xxx
// 使用已保存供应商的 base_url、第一条 API Key 及自定义请求头。
func (s *Server) handleFetchModelsByID(w http.ResponseWriter, r *http.Request) {
	pid := r.URL.Query().Get("provider_id")
	p := s.State.FindProvider(pid)
	if p == nil {
		writeErr(w, http.StatusNotFound, "provider not found", "not_found")
		return
	}
	if len(p.APIKeys) == 0 {
		writeErr(w, http.StatusBadRequest, "该供应商尚未配置 API Key", "invalid_request")
		return
	}
	// v1.8.5（G5）：随请求 ctx 取消（客户端断开即终止，不再依赖 60s 超时兜底）
	models, err := fetchUpstreamModels(r.Context(), p.APIType, p.BaseURL, p.Headers, p.APIKeys[0].Key, s.State.Snap().DefaultUserAgent, 60*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "获取模型失败："+err.Error(), "fetch_models_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"models": models})
}

// handleFetchModelsForm POST /api/providers/fetch-models
// 表单模式（新增页 / 编辑页统一，v1.8.3 F1）：使用表单当前填写的数据，按顺序依次尝试各 Key。
// 编辑页表单中的 Key 为脱敏值（含 ***），按 provider_id + key id 从已保存配置恢复完整值
// （与 /api/providers/test 同模式）；无法恢复的跳过。
func (s *Server) handleFetchModelsForm(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProviderID string `json:"provider_id"`
		BaseURL    string `json:"base_url"`
		APIType    string `json:"api_type"`
		APIKeys    []struct {
			ID  string `json:"id"`
			Key string `json:"key"`
		} `json:"api_keys"`
		Headers []config.ProviderHeader `json:"headers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body", "invalid_request")
		return
	}
	base := strings.TrimSuffix(strings.TrimSpace(body.BaseURL), "/")
	if base == "" {
		writeErr(w, http.StatusBadRequest, "基础地址不能为空", "invalid_request")
		return
	}
	// v1.8.4（A11）：与 CRUD 表单校验同口径
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		writeErr(w, http.StatusBadRequest, "基础地址需以 http:// 或 https:// 开头", "invalid_request")
		return
	}
	// v1.8.4（M9）：Key 数量上限——与 handleProviderTest 的 L12 同口径；不设上限时 8MB
	// 请求体内可塞入约十余万条 Key 目标（每条约 40-50 字节 JSON），串行尝试（每条
	// 60s 超时）可把请求挂起数天；L12 只给测试接口加了上限，此处漏做。
	const maxFetchKeys = 50
	if len(body.APIKeys) > maxFetchKeys {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("单次最多尝试 %d 条 API Key", maxFetchKeys), "invalid_request")
		return
	}
	// 已保存供应商（用于恢复编辑页表单中的脱敏 Key）
	var saved *config.Provider
	if body.ProviderID != "" {
		saved = s.State.FindProvider(body.ProviderID)
	}
	// v1.8.5（G5，关联检查报告 B1）：整体时间预算——表单模式串行尝试至多 50 Key、
	// 每 Key 60s 超时，最坏可挂起约 50 分钟，与 handleProviderTest 的 v1.8.4（A2）
	// 预算不对称；以 r.Context() 派生 10 分钟总预算，超预算后停止尝试剩余 Key
	//（客户端断开同样会提前终止进行中的请求）。
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	var lastErr error
	for _, k := range body.APIKeys {
		key := strings.TrimSpace(k.Key)
		if key == "" {
			continue
		}
		// v1.8.4（A9）：提交值恰为现有 Key 的脱敏值时视为未修改，恢复完整值；
		// 否则按新输入处理（真实 Key 含 *** 不再被误跳过）
		if saved != nil {
			if full, ok := maskedRestoreByKeyID(key, k.ID, saved.APIKeys); ok {
				key = full
			}
		}
		if ctx.Err() != nil {
			if lastErr == nil {
				lastErr = fmt.Errorf("总超时（10 分钟预算耗尽），已停止尝试剩余 Key")
			} else {
				lastErr = fmt.Errorf("总超时（10 分钟预算耗尽），已停止尝试剩余 Key（最后错误：%s）", lastErr.Error())
			}
			break
		}
		models, err := fetchUpstreamModels(ctx, body.APIType, base, body.Headers, key, s.State.Snap().DefaultUserAgent, 60*time.Second)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]interface{}{"models": models})
			return
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的 API Key")
	}
	writeErr(w, http.StatusBadGateway, "获取模型失败："+lastErr.Error(), "fetch_models_failed")
}

// truncateStr 按字节上限截断字符串，附加 "..." 标记。
// v1.8.3（L11）：截断点回退到最近的 UTF-8 字符边界——与 proxy 包 truncate 的 C5 修复同口径，
// 上游错误消息含中文时不再截出乱码尾字节。
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	end := n
	for end > 0 && !utf8.RuneStart(s[end]) {
		end-- // 回退到多字节字符的起始边界
	}
	return s[:end] + "..."
}
