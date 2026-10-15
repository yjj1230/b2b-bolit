package wecom

import (
	"context"
	"encoding/json"
	"fmt"
)

// ContextBindingStore 群上下文绑定读取契约（repo.Repo.LookupBinding 实现）。
// 测试/生产环境通过 wecom_context_bindings 表注入「客户群 → 客户 + 归属销售」。
type ContextBindingStore interface {
	LookupBinding(ctx context.Context, groupID string) (tenantID string, binding []byte, err error)
}

// OfficialIdentityProvider official OAuth：code → userid（服务端重新认证），
// chat_id → wecom_context_bindings 客户上下文；不信任前端传入身份。
type OfficialIdentityProvider struct {
	gw       *OfficialGateway
	bindings ContextBindingStore
}

// NewOfficialIdentityProvider 构造 official 身份提供者。
func NewOfficialIdentityProvider(gw *OfficialGateway, bindings ContextBindingStore) *OfficialIdentityProvider {
	return &OfficialIdentityProvider{gw: gw, bindings: bindings}
}

// Exchange code + chatID → principal/tenant/customer（企微 OAuth + 群上下文绑定）。
func (p *OfficialIdentityProvider) Exchange(ctx context.Context, code, chatID string) (ExchangedContext, error) {
	if p.gw == nil {
		return ExchangedContext{}, fmt.Errorf("official identity: gateway not configured")
	}
	cfg, err := p.gw.TenantConfig()
	if err != nil {
		return ExchangedContext{}, err
	}
	userID, err := p.gw.UserIDByCode(ctx, code)
	if err != nil {
		return ExchangedContext{}, err
	}
	out := ExchangedContext{
		PrincipalID: userID,
		TenantID:    cfg.TenantID,
	}
	if chatID == "" {
		return out, nil
	}
	if p.bindings == nil {
		return ExchangedContext{}, fmt.Errorf("official identity: chat %q 需要 wecom_context_bindings，未配置 binding store", chatID)
	}
	tenantID, raw, err := p.bindings.LookupBinding(ctx, chatID)
	if err != nil {
		return ExchangedContext{}, fmt.Errorf("official identity: chat %q 未绑定客户: %w", chatID, err)
	}
	var b struct {
		CustomerID  string `json:"customer_id"`
		OwnerUserID string `json:"owner_user_id"`
		OwnerName   string `json:"owner_name"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return ExchangedContext{}, fmt.Errorf("official identity: parse chat binding: %w", err)
	}
	if b.CustomerID == "" {
		return ExchangedContext{}, fmt.Errorf("official identity: chat %q 绑定缺少 customer_id", chatID)
	}
	out.TenantID = tenantID
	out.CustomerID = b.CustomerID
	out.CustomerManual = true
	return out, nil
}
