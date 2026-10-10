package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"llmproxy/internal/config"
	"llmproxy/internal/core"
)

// apiKeyPayload 代理 Key 表单。
type apiKeyPayload struct {
	Name        string   `json:"name"`
	Key         string   `json:"key"`
	ExpiresAt   *string  `json:"expires_at"`
	IPWhitelist []string `json:"ip_whitelist"`
	Enabled     *bool    `json:"enabled"`
	Models      []string `json:"models"`
}

// apiKeyView 列表视图（Key 脱敏）。
type apiKeyView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Key         string   `json:"key"` // 脱敏
	ExpiresAt   string   `json:"expires_at,omitempty"`
	IPWhitelist []string `json:"ip_whitelist"`
	Enabled     bool     `json:"enabled"`
	Models      []string `json:"models"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

func toKeyView(k *config.ProxyKey) *apiKeyView {
	return &apiKeyView{
		ID: k.ID, Name: k.Name, Key: core.MaskKey(k.Key),
		ExpiresAt: k.ExpiresAt, IPWhitelist: k.IPWhitelist, Enabled: k.Enabled,
		Models: k.Models, CreatedAt: k.CreatedAt, UpdatedAt: k.UpdatedAt,
	}
}

func (s *Server) handleAPIKeyList(w http.ResponseWriter, r *http.Request) {
	snap := s.State.Snap()
	out := make([]*apiKeyView, 0, len(snap.APIKeys))
	for _, k := range snap.APIKeys {
		out = append(out, toKeyView(k))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"apikeys": out})
}

// handleAPIKeyGet 详情：返回完整 Key（供前端「显示」与「复制」使用）。
func (s *Server) handleAPIKeyGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	k := s.State.FindProxyKeyByID(id)
	if k == nil {
		writeErr(w, http.StatusNotFound, "api key not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id": k.ID, "name": k.Name, "key": k.Key,
		"expires_at": k.ExpiresAt, "ip_whitelist": k.IPWhitelist,
		"enabled": k.Enabled, "models": k.Models,
		"created_at": k.CreatedAt, "updated_at": k.UpdatedAt,
	})
}

func (s *Server) validateProxyKeyForm(body *apiKeyPayload, existingID, existingKey string) (string, error) {
	if strings.TrimSpace(body.Name) == "" {
		return "", fmtError("名称不能为空")
	}
	key := strings.TrimSpace(body.Key)
	if key == "" {
		return "", fmtError("API Key 不能为空")
	}
	// 格式校验（需求 2.7.2）：sk- 前缀 + 32 位字母数字。
	// 编辑时 Key 未变（== existingKey，含脱敏值恢复后的原值）则跳过——
	// 存量手写配置的非标准 Key 仍可编辑其他字段，不因历史数据被拒（v1.0.0 L2 修复）。
	if key != existingKey && !validProxyKeyFormat(key) {
		return "", fmtError("API Key 需为 sk- 前缀 + 32 位字母数字（a-zA-Z0-9），或点击「随机生成」")
	}
	// 全局唯一性
	for _, k := range s.State.Snap().APIKeys {
		if k.Key == key && k.ID != existingID {
			return "", fmtError("该 API Key 已存在（全局唯一）")
		}
	}
	if body.ExpiresAt != nil && strings.TrimSpace(*body.ExpiresAt) != "" {
		normalized, err := normalizeExpiresAt(*body.ExpiresAt)
		if err != nil {
			return "", err
		}
		body.ExpiresAt = &normalized
	}
	return key, nil
}

// validProxyKeyFormat 校验代理 Key 格式：sk- + 32 位 [a-zA-Z0-9]（需求 2.7.2）。
func validProxyKeyFormat(k string) bool {
	if !strings.HasPrefix(k, "sk-") {
		return false
	}
	rest := k[len("sk-"):]
	if len(rest) != 32 {
		return false
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// normalizeExpiresAt 归一化有效期输入，统一输出 RFC3339（带时区）。
// 接受格式：
//   - RFC3339（YYYY-MM-DDTHH:MM:SS±HH:MM / Z）→ 原样保留
//   - 纯日期 YYYY-MM-DD → 当天 23:59:59（本地时区）
//   - 无时区日期时间 YYYY-MM-DDTHH:MM[:SS]（如 datetime-local 输出）→ 按本地时区补全
func normalizeExpiresAt(input string) (string, error) {
	t := strings.TrimSpace(input)
	if t == "" {
		return "", nil
	}
	if ts, err := time.Parse(time.RFC3339, t); err == nil {
		return ts.Format(time.RFC3339), nil
	}
	if ts, err := time.ParseInLocation("2006-01-02", t, time.Local); err == nil {
		ts = time.Date(ts.Year(), ts.Month(), ts.Day(), 23, 59, 59, 0, time.Local)
		return ts.Format(time.RFC3339), nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if ts, err := time.ParseInLocation(layout, t, time.Local); err == nil {
			return ts.Format(time.RFC3339), nil
		}
	}
	return "", fmtError("有效期需为 ISO 8601（YYYY-MM-DD 或 YYYY-MM-DDTHH:MM:SS）格式")
}

func fmtError(msg string) error { return &validationError{msg} }

type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }

func (s *Server) handleAPIKeyCreate(w http.ResponseWriter, r *http.Request) {
	var body apiKeyPayload
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body", "invalid_request")
		return
	}
	key, err := s.validateProxyKeyForm(&body, "", "")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}
	now := time.Now().Format(time.RFC3339)
	wl := body.IPWhitelist
	if wl == nil || len(wl) == 0 {
		wl = []string{"*"}
	}
	models := []string{}
	for _, m := range body.Models {
		m = strings.TrimSpace(m)
		if m != "" {
			models = append(models, m)
		}
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	nk := &config.ProxyKey{
		ID: core.NewID(), Name: strings.TrimSpace(body.Name), Key: key,
		ExpiresAt: body.ExpiresAtSafe(), IPWhitelist: wl, Enabled: enabled,
		Models: models, CreatedAt: now, UpdatedAt: now,
	}
	var dupErr error // v1.0.0（L3）：临界区内复查唯一性，消除锁外预检的 TOCTOU 窗口
	if err := s.State.Mutate(func(ns *core.Snapshot) {
		// v1.0.0（L3）：唯一性校验在 Mutate 临界区内复查——锁外预检与提交之间存在窗口，
		// 两个并发 create 同 Key 可双双通过预检产生重复配置（FindProxyKey 只命中第一条，第二条成死配置）。
		for _, k := range ns.APIKeys {
			if k.Key == key {
				dupErr = fmtError("该 API Key 已存在（全局唯一）")
				return
			}
		}
		ns.APIKeys = append(ns.APIKeys, nk)
	}); err != nil || dupErr != nil {
		if dupErr != nil {
			writeErr(w, http.StatusBadRequest, dupErr.Error(), "invalid_request")
			return
		}
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error(), "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, toKeyView(nk))
}

// ExpiresAtSafe 获取清理后的过期时间。
func (p *apiKeyPayload) ExpiresAtSafe() string {
	if p.ExpiresAt == nil {
		return ""
	}
	return strings.TrimSpace(*p.ExpiresAt)
}

func (s *Server) handleAPIKeyUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body apiKeyPayload
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body", "invalid_request")
		return
	}
	// 编辑时 key 可能为脱敏回显值 → 保留原值。
	// v1.0.0（A9）：精确比对取代 Contains("***")——真实代理 Key 若字面含 ***（理论上
	// sk-+32 位随机不会，但防御性口径与供应商侧统一），内容特征误判会静默把用户输入
	// 替换回旧值；只有提交值恰为现有 Key 的脱敏值时才视为未修改。
	existing := s.State.FindProxyKeyByID(id)
	if existing == nil {
		writeErr(w, http.StatusNotFound, "api key not found", "not_found")
		return
	}
	if body.Key == core.MaskKey(existing.Key) {
		body.Key = existing.Key
	}
	key, err := s.validateProxyKeyForm(&body, id, existing.Key)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}
	updated := false
	var dupErr error                // v1.0.0（L3）：临界区内复查同 Create
	var updatedKey *config.ProxyKey // v1.0.0（A4）：锁内捕获更新后的 Key 指针——旧实现响应阶段
	// 再次 FindProxyKeyByID，并发 DELETE 同一 Key 恰好落在窗口内时返回 nil、toKeyView
	// 解引用空指针 panic（net/http recover 后客户端收到连接中断而非统一 JSON 错误）。
	if err := s.State.Mutate(func(ns *core.Snapshot) {
		for _, k := range ns.APIKeys {
			if k.Key == key && k.ID != id {
				dupErr = fmtError("该 API Key 已存在（全局唯一）")
				return
			}
		}
		for _, k := range ns.APIKeys {
			if k.ID == id {
				k.Name = strings.TrimSpace(body.Name)
				k.Key = key
				k.ExpiresAt = body.ExpiresAtSafe()
				wl := body.IPWhitelist
				if wl == nil || len(wl) == 0 {
					wl = []string{"*"}
				}
				k.IPWhitelist = wl
				if body.Enabled != nil {
					k.Enabled = *body.Enabled
				}
				models := []string{}
				for _, m := range body.Models {
					m = strings.TrimSpace(m)
					if m != "" {
						models = append(models, m)
					}
				}
				k.Models = models
				k.UpdatedAt = time.Now().Format(time.RFC3339)
				updated = true
				updatedKey = k
				return
			}
		}
	}); err != nil || dupErr != nil {
		if dupErr != nil {
			writeErr(w, http.StatusBadRequest, dupErr.Error(), "invalid_request")
			return
		}
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error(), "internal_error")
		return
	}
	if !updated || updatedKey == nil {
		writeErr(w, http.StatusNotFound, "api key not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, toKeyView(updatedKey))
}

func (s *Server) handleAPIKeyDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	found := false
	if err := s.State.Mutate(func(ns *core.Snapshot) {
		out := ns.APIKeys[:0]
		for _, k := range ns.APIKeys {
			if k.ID == id {
				found = true
				continue
			}
			out = append(out, k)
		}
		ns.APIKeys = out
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "删除失败: "+err.Error(), "internal_error")
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "api key not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAPIKeyEnable(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		found := false
		if err := s.State.Mutate(func(ns *core.Snapshot) {
			for _, k := range ns.APIKeys {
				if k.ID == id {
					k.Enabled = enabled
					k.UpdatedAt = time.Now().Format(time.RFC3339)
					found = true
					return
				}
			}
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, "操作失败: "+err.Error(), "internal_error")
			return
		}
		if !found {
			writeErr(w, http.StatusNotFound, "api key not found", "not_found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}
