//go:build integration

package e2e

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/tests/testkit"
)

// TestE2EWeComS1Sidebar W6 · 企微 S1 侧边栏 H5 服务端链路（07 §6.1）
//
// 覆盖：jsapi-config 双签名（协议白名单）→ OAuth exchange（code+chat → 身份/上下文 +
// 服务端签发 context_token）→ X-WeCom-Context 校验（篡改/principal 不一致拒绝）→
// 携带上下文查订单（BFF 投影）。与 v2 apps/wecom-miniapp + bff-api 的调用序列对齐。
func TestE2EWeComS1Sidebar(t *testing.T) {
	e := testkit.NewEnv(t)

	// ── ① jsapi-config：双签名结构 + 协议白名单 ──
	rec := e.DoJSON("GET", "/api/v1/wecom/jsapi-config?url="+
		"https%3A%2F%2Fwecom.changan.example.com%2Fprestock%2Fcreate", "", http.StatusOK, nil)
	var js struct {
		CorpID         string `json:"corp_id"`
		AgentID        string `json:"agent_id"`
		Signature      string `json:"signature"`
		AgentSignature string `json:"agent_signature"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &js))
	require.Equal(t, "ww-chang-an-demo", js.CorpID)
	require.NotEmpty(t, js.Signature, "wx.config 企业签名")
	require.NotEmpty(t, js.AgentSignature, "wx.agentConfig 应用签名")

	// 协议白名单：非 http/https 拒绝
	bad := e.Do("GET", "/api/v1/wecom/jsapi-config?url=javascript%3Aalert(1)", "", nil)
	require.Equal(t, http.StatusBadRequest, bad.Code)

	// ── ② OAuth exchange：code + chat → 身份/上下文 + context_token ──
	rec2 := e.DoJSON("POST", "/api/v1/auth/exchange",
		`{"code":"mock-supervisor","chat_id":"chat-cust-0001"}`, http.StatusOK, nil)
	var ex struct {
		PrincipalID    string `json:"principal_id"`
		TenantID       string `json:"tenant_id"`
		CustomerID     string `json:"customer_id"`
		ContextToken   string `json:"context_token"`
		CustomerManual bool   `json:"customer_manual"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &ex))
	require.Equal(t, "u-supervisor", ex.PrincipalID)
	require.Equal(t, "chang-an", ex.TenantID)
	require.Equal(t, "cust-0001", ex.CustomerID)
	require.True(t, ex.CustomerManual)
	require.NotEmpty(t, ex.ContextToken)

	// 未知 code → 明确错误
	e.DoJSON("POST", "/api/v1/auth/exchange", `{"code":"bogus","chat_id":"chat-cust-0001"}`, http.StatusBadRequest, nil)

	// ── ③ 上下文校验：有效 token → 业务请求 200；篡改 → 401；principal 不一致 → 401 ──
	orderID := e.CreateOrder("款到", "order:chang-an:quick:e2e-w6-s1-1")
	ctxHeaders := map[string]string{
		"X-User-Id":       ex.PrincipalID,
		"X-WeCom-Context": ex.ContextToken,
	}
	rec3 := e.Do("GET", "/api/v1/orders/"+orderID, "", ctxHeaders)
	require.Equal(t, http.StatusOK, rec3.Code)
	// 带上下文请求正常（本地工作流异步推进，等待审价闸门）
	e.WaitOrderState(orderID, domain.StateORDPriceReviewing)

	// 篡改 token → 401
	e.Do("GET", "/api/v1/orders/"+orderID, "",
		map[string]string{"X-User-Id": ex.PrincipalID, "X-WeCom-Context": ex.ContextToken + "x"})
	// 上句仅 Do 不断言；正式断言：
	tampered := e.Do("GET", "/api/v1/orders/"+orderID, "",
		map[string]string{"X-User-Id": ex.PrincipalID, "X-WeCom-Context": ex.ContextToken[:10] + "X" + ex.ContextToken[11:]})
	require.Equal(t, http.StatusUnauthorized, tampered.Code)

	// ROL-01：身份以签名令牌 principal 为准，忽略请求头（X-User-Id 不再能冒名）
	other := e.Do("GET", "/api/v1/orders/"+orderID, "",
		map[string]string{"X-User-Id": "u-wh-yangyuan", "X-WeCom-Context": ex.ContextToken})
	require.Equal(t, http.StatusOK, other.Code)

	// ── ④ 客户上下文 + 权限（场景 D）：contact 视角投影（剥金额）──
	recC := e.DoJSON("POST", "/api/v1/auth/exchange",
		`{"code":"mock-contact","chat_id":"chat-cust-0001"}`, http.StatusOK, nil)
	var exC struct {
		PrincipalID  string `json:"principal_id"`
		ContextToken string `json:"context_token"`
	}
	require.NoError(t, json.Unmarshal(recC.Body.Bytes(), &exC))
	contactView := e.Do("GET", "/api/v1/orders/"+orderID, "",
		map[string]string{"X-User-Id": exC.PrincipalID, "X-WeCom-Context": exC.ContextToken})
	require.Equal(t, http.StatusOK, contactView.Code)
	var cview map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(contactView.Body.Bytes(), &cview))
	_, hasAmount := cview["amount_cents"]
	_, hasPrice := cview["unit_price_cents"]
	require.False(t, hasAmount, "客户联系人订单视图应剥离金额（07 §4.1）")
	require.False(t, hasPrice, "客户联系人订单视图应剥离单价（07 §4.1）")
}
