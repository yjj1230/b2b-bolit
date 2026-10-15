package wecom

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ── S1 侧边栏服务端能力（07 §6.1；对齐 v2 FakeWeCom 语义）──

func TestJSAPIConfigBuilder(t *testing.T) {
	b := NewJSAPIConfigBuilder("ww-chang-an-demo", "1000002", "seed-1")

	// 正常构建：双签名结构完整、确定性（同输入同输出）
	cfg, err := b.BuildJSAPIConfig(context.Background(), "https://wecom.changan.example.com/order/ORD-1")
	require.NoError(t, err)
	require.Equal(t, "ww-chang-an-demo", cfg.CorpID)
	require.Equal(t, "1000002", cfg.AgentID)
	require.NotEmpty(t, cfg.Signature)      // wx.config 企业签名
	require.NotEmpty(t, cfg.AgentSignature) // wx.agentConfig 应用签名
	require.Equal(t, cfg.Timestamp, cfg.AgentTimestamp)
	require.Equal(t, cfg.NonceStr, cfg.AgentNonceStr)

	cfg2, err := b.BuildJSAPIConfig(context.Background(), "https://wecom.changan.example.com/order/ORD-1")
	require.NoError(t, err)
	require.Equal(t, cfg.Signature, cfg2.Signature, "mock 签名应确定性（同 url 同签名）")

	// 协议白名单：非 http/https 拒绝（对齐 v2 handleJSAPIConfig）
	for _, bad := range []string{"javascript:alert(1)", "file:///etc/passwd", "ftp://x/y", "https://", "not-a-url"} {
		_, err := b.BuildJSAPIConfig(context.Background(), bad)
		require.Error(t, err, bad)
	}
}

func TestMockIdentityProviderExchange(t *testing.T) {
	t.Setenv("WECOM_CHAT_BINDINGS", "chat-cust-9999=cust-0001")
	p := NewMockIdentityProvider("chang-an", map[string]string{
		"mock-supervisor": "u-supervisor",
		"mock-warehouse":  "u-wh-yangyuan",
	}, map[string]string{"chat-cust-0001": "cust-0001"})

	// code + chat → 身份与客户上下文
	ex, err := p.Exchange(context.Background(), "mock-supervisor", "chat-cust-0001")
	require.NoError(t, err)
	require.Equal(t, "u-supervisor", ex.PrincipalID)
	require.Equal(t, "chang-an", ex.TenantID)
	require.Equal(t, "cust-0001", ex.CustomerID)
	require.True(t, ex.CustomerManual)

	// chat 为空（内部群无客户上下文）→ 允许，无 customer
	ex2, err := p.Exchange(context.Background(), "mock-warehouse", "")
	require.NoError(t, err)
	require.Equal(t, "u-wh-yangyuan", ex2.PrincipalID)
	require.Equal(t, "", ex2.CustomerID)

	// 环境变量追加绑定（v2 支持 WECOM_CHAT_BINDINGS）
	ex3, err := p.Exchange(context.Background(), "mock-supervisor", "chat-cust-9999")
	require.NoError(t, err)
	require.Equal(t, "cust-0001", ex3.CustomerID)

	// 未知 code / 未绑定 chat → 明确错误（不静默降级）
	_, err = p.Exchange(context.Background(), "bogus-code", "chat-cust-0001")
	require.Error(t, err)
	_, err = p.Exchange(context.Background(), "mock-supervisor", "chat-unknown")
	require.Error(t, err)
}

func TestContextIssuer(t *testing.T) {
	i := NewContextIssuer("test-secret", time.Hour)

	// 签发 → 校验往返
	tok, err := i.Issue("u-supervisor", "chang-an", "cust-0001")
	require.NoError(t, err)
	p, tenant, cust, ok := i.Verify(tok)
	require.True(t, ok)
	require.Equal(t, "u-supervisor", p)
	require.Equal(t, "chang-an", tenant)
	require.Equal(t, "cust-0001", cust)

	// 篡改 payload / 签名 → 拒绝
	tampered := tok[:10] + "X" + tok[11:]
	_, _, _, ok = i.Verify(tampered)
	require.False(t, ok)
	_, _, _, ok = i.Verify("bogus")
	require.False(t, ok)

	// 过期 → 拒绝（ttl 极小 + 纳秒精度比较，毫秒级即过期）
	short := NewContextIssuer("test-secret", time.Millisecond)
	oldTok, err := short.Issue("u-supervisor", "chang-an", "")
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	_, _, _, ok = short.Verify(oldTok)
	require.False(t, ok)

	// 密钥不匹配（另一签发者）→ 拒绝
	i2 := NewContextIssuer("other-secret", time.Hour)
	_, _, _, ok = i2.Verify(tok)
	require.False(t, ok)
}

// TestJSAPIConfigDeterminismAcrossTime 时间不影响 mock 签名确定性断言：
// 同 url 在同一秒内签名一致（签名公式含 timestamp——确定性仅限同秒；跨秒签名变化
// 属预期。此测试验证结构而非值相等）。
func TestJSAPIConfigStructure(t *testing.T) {
	b := NewJSAPIConfigBuilder("ww-x", "1000002", "s")
	cfg, err := b.BuildJSAPIConfig(context.Background(), "https://a.example.com/")
	require.NoError(t, err)
	require.Len(t, cfg.Signature, 40) // sha1 hex = 40
	require.Len(t, cfg.AgentSignature, 40)
}
