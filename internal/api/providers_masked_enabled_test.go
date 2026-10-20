package api

import (
	"testing"

	"llmproxy/internal/config"
	"llmproxy/internal/core"
)

func boolPtr(b bool) *bool { return &b }

// maskedKeyAPIKeyItem 与 providerPayload.APIKeys 的元素类型保持一致。
type maskedKeyAPIKeyItem = struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	Enabled *bool  `json:"enabled"`
}

// TestMaskedKeyRespectsExplicitEnabledOff // 编辑供应商时仅显式把脱敏回显的 Key 设为 enabled=false，旧 enabled=true 不得覆盖。
func TestMaskedKeyRespectsExplicitEnabledOff(t *testing.T) {
	const realKey = "sk-th1s1sareallongkey"
	masked := core.MaskKey(realKey)

	old := &config.Provider{
		ID:      "p1",
		Name:    "p1",
		BaseURL: "https://api.example.com",
		APIKeys: []config.ProviderAPIKey{{ID: "k1", Key: realKey, Enabled: true}},
	}
	body := providerPayload{
		Name:    "p1",
		BaseURL: "https://api.example.com",
		APIKeys: []maskedKeyAPIKeyItem{{ID: "k1", Key: masked, Enabled: boolPtr(false)}},
	}
	applyPayload(old, &body, false)
	if len(old.APIKeys) != 1 {
		t.Fatalf("期望 1 个 Key，实际 %d", len(old.APIKeys))
	}
	if old.APIKeys[0].Key != realKey {
		t.Fatalf("脱敏 Key 未恢复完整值: %q", old.APIKeys[0].Key)
	}
	if old.APIKeys[0].Enabled != false {
		t.Fatalf("期望 enabled=false（表单显式值），实际 %v", old.APIKeys[0].Enabled)
	}
}

// TestMaskedKeyRespectsExplicitEnabledOn // 旧 enabled=false，编辑时显式 enabled=true，应生效而非被旧值覆盖。
func TestMaskedKeyRespectsExplicitEnabledOn(t *testing.T) {
	const realKey = "sk-th1s1sareallongkey"
	masked := core.MaskKey(realKey)

	old := &config.Provider{
		ID:      "p1",
		Name:    "p1",
		BaseURL: "https://api.example.com",
		APIKeys: []config.ProviderAPIKey{{ID: "k1", Key: realKey, Enabled: false}},
	}
	body := providerPayload{
		Name:    "p1",
		BaseURL: "https://api.example.com",
		APIKeys: []maskedKeyAPIKeyItem{{ID: "k1", Key: masked, Enabled: boolPtr(true)}},
	}
	applyPayload(old, &body, false)
	if old.APIKeys[0].Key != realKey {
		t.Fatalf("脱敏 Key 未恢复完整值: %q", old.APIKeys[0].Key)
	}
	if old.APIKeys[0].Enabled != true {
		t.Fatalf("期望 enabled=true（表单显式值），实际 %v", old.APIKeys[0].Enabled)
	}
}

// TestMaskedKeyNoExplicitEnabledKeepsOld // 脱敏回显 Key 且未显式传 enabled（nil），应沿用已保存旧值。
func TestMaskedKeyNoExplicitEnabledKeepsOld(t *testing.T) {
	const realKey = "sk-th1s1sareallongkey"
	masked := core.MaskKey(realKey)

	old := &config.Provider{
		ID:      "p1",
		Name:    "p1",
		BaseURL: "https://api.example.com",
		APIKeys: []config.ProviderAPIKey{{ID: "k1", Key: realKey, Enabled: false}},
	}
	body := providerPayload{
		Name:    "p1",
		BaseURL: "https://api.example.com",
		APIKeys: []maskedKeyAPIKeyItem{{ID: "k1", Key: masked, Enabled: nil}},
	}
	applyPayload(old, &body, false)
	if old.APIKeys[0].Enabled != false {
		t.Fatalf("未传 enabled 应沿用旧值 false，实际 %v", old.APIKeys[0].Enabled)
	}
}
