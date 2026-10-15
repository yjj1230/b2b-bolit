package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/wecom"
	"b2b_pilot_v3/internal/workflows"
)

// ── S1 端点单测（make cover 覆盖源；E2E 另由 wecom_s1_e2e_test.go 兜底）──

func s1TestServer(t *testing.T) *Server {
	store := workflows.NewFakeStoreForTest()
	seedAPIWorld(t, store)
	s := NewServer(store, nil, nil, func(userID string) (authz.Role, string) {
		return authz.RoleSupervisor, "chang-an"
	})
	s.JSAPI = wecom.NewJSAPIConfigBuilder("ww-chang-an-demo", "1000002", "test-seed")
	s.Identity = wecom.NewMockIdentityProvider("chang-an", map[string]string{"mock-supervisor": "u-supervisor"}, map[string]string{"chat-cust-0001": "cust-0001"})
	s.Context = wecom.NewContextIssuer("test-secret", 0)
	return s
}

func TestS1JSAPIConfigHandler(t *testing.T) {
	s := s1TestServer(t)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()

	// 正常：双签名结构
	resp, err := http.Get(ts.URL + "/api/v1/wecom/jsapi-config?url=https%3A%2F%2Fwecom.example.com%2Fx")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var cfg wecom.JSAPIConfig
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&cfg))
	resp.Body.Close()
	require.NotEmpty(t, cfg.Signature)
	require.NotEmpty(t, cfg.AgentSignature)

	// 协议白名单拒绝
	resp2, err := http.Get(ts.URL + "/api/v1/wecom/jsapi-config?url=javascript%3Aalert(1)")
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp2.StatusCode)
	resp2.Body.Close()
}

func TestS1ExchangeHandler(t *testing.T) {
	s := s1TestServer(t)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()

	// 正常交换：身份 + 上下文 + token
	resp, err := http.Post(ts.URL+"/api/v1/auth/exchange", "application/json",
		strings.NewReader(`{"code":"mock-supervisor","chat_id":"chat-cust-0001"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var ex wecom.ExchangedContext
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&ex))
	resp.Body.Close()
	require.Equal(t, "u-supervisor", ex.PrincipalID)
	require.Equal(t, "cust-0001", ex.CustomerID)
	require.NotEmpty(t, ex.ContextToken)

	// 未知 code → 400
	resp2, err := http.Post(ts.URL+"/api/v1/auth/exchange", "application/json", strings.NewReader(`{"code":"bogus"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp2.StatusCode)
	resp2.Body.Close()
}

func TestS1ContextValidation(t *testing.T) {
	s := s1TestServer(t)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()

	// 签发有效 token
	resp, err := http.Post(ts.URL+"/api/v1/auth/exchange", "application/json", strings.NewReader(`{"code":"mock-supervisor","chat_id":"chat-cust-0001"}`))
	require.NoError(t, err)
	var ex wecom.ExchangedContext
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&ex))
	resp.Body.Close()

	// 无 X-WeCom-Context 的业务请求不受影响（既有调用零影响）
	req, _ := http.NewRequest("GET", ts.URL+"/healthz", nil)
	r, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, r.StatusCode)
	r.Body.Close()

	// 带有效 token 的业务请求通过（健康检查不带 user 校验；用 todos 验证上下文注入不拒绝）
	req2, _ := http.NewRequest("GET", ts.URL+"/api/v1/todos", nil)
	req2.Header.Set("X-User-Id", "u-supervisor")
	req2.Header.Set("X-WeCom-Context", ex.ContextToken)
	r2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, r2.StatusCode)
	r2.Body.Close()

	// 篡改 token → 401
	req3, _ := http.NewRequest("GET", ts.URL+"/api/v1/todos", nil)
	req3.Header.Set("X-User-Id", "u-supervisor")
	req3.Header.Set("X-WeCom-Context", ex.ContextToken+"x")
	r3, err := http.DefaultClient.Do(req3)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, r3.StatusCode)
	r3.Body.Close()
}

func TestS1NotEnabled(t *testing.T) {
	// 未装配 S1 能力 → 501（防御分支）
	s := NewServer(nil, nil, nil, func(userID string) (authz.Role, string) {
		return authz.RoleSupervisor, "chang-an"
	})
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/wecom/jsapi-config?url=https%3A%2F%2Fwecom.example.com%2Fx")
	require.NoError(t, err)
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	resp.Body.Close()
	resp2, err := http.Post(ts.URL+"/api/v1/auth/exchange", "application/json", strings.NewReader(`{"code":"x"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusNotImplemented, resp2.StatusCode)
	resp2.Body.Close()
}
