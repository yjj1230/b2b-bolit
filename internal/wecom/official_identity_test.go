package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/config"
)

type fakeBindingStore struct {
	tenant  string
	binding []byte
	err     error
}

func (f *fakeBindingStore) LookupBinding(ctx context.Context, groupID string) (string, []byte, error) {
	return f.tenant, f.binding, f.err
}

func writeTenantCfg(t *testing.T, dir, name, domain, corp string, agent int) string {
	t.Helper()
	raw := map[string]any{
		"tenant_id": "chang-an",
		"corp_id":   corp,
		"domain":    domain,
		"app": map[string]any{
			"agent_id":         agent,
			"secret":           "s",
			"token":            "tok",
			"encoding_aes_key": "0123456789abcdef0123456789abcdef0123456",
		},
	}
	b, _ := json.Marshal(raw)
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, b, 0o600))
	return p
}

func TestOfficialGroupSendTicketsAndUserID(t *testing.T) {
	var appchatBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "access_token": "tok-1", "expires_in": 7200})
		case "/cgi-bin/appchat/send":
			_ = json.NewDecoder(r.Body).Decode(&appchatBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0})
		case "/cgi-bin/get_jsapi_ticket":
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "ticket": "jsapi-t-1", "expires_in": 7200})
		case "/cgi-bin/ticket/get":
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "ticket": "agent-t-1", "expires_in": 7200})
		case "/cgi-bin/user/getuserinfo":
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "userid": "hongli"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	o := &OfficialGateway{httpc: srv.Client(), baseURL: srv.URL, cfg: config.Config{WeComProvider: "official"}}
	o.token, o.tokenExp = "", time.Time{}

	mid, err := o.SendGroupCard(context.Background(), "chang-an", "group-1", Card{Kind: "ORDER_EVENT", State: "已开单", OrderID: "ORD-1"})
	require.NoError(t, err)
	require.Equal(t, "group-1", mid)
	require.Equal(t, "group-1", appchatBody["chatid"])

	jt, err := o.JSAPITicket(context.Background())
	require.NoError(t, err)
	require.Equal(t, "jsapi-t-1", jt)
	at, err := o.AgentConfigTicket(context.Background())
	require.NoError(t, err)
	require.Equal(t, "agent-t-1", at)

	uid, err := o.UserIDByCode(context.Background(), "code-1")
	require.NoError(t, err)
	require.Equal(t, "hongli", uid)

	// 错误分支：无 groupID / errcode != 0
	_, err = o.SendGroupCard(context.Background(), "chang-an", "", Card{})
	require.Error(t, err)

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 40001, "errmsg": "bad token"})
	}))
	defer bad.Close()
	o2 := &OfficialGateway{httpc: bad.Client(), baseURL: bad.URL, cfg: config.Config{WeComProvider: "official"}}
	_, err = o2.SendGroupCard(context.Background(), "chang-an", "g", Card{})
	require.Error(t, err)
	_, err = o2.JSAPITicket(context.Background())
	require.Error(t, err)
	_, err = o2.AgentConfigTicket(context.Background())
	require.Error(t, err)
	_, err = o2.UserIDByCode(context.Background(), "c")
	require.Error(t, err)
}

func TestOfficialJSAPIAndIdentityProviders(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTenantCfg(t, dir, "tenant.json", "wecom.changan.example.com", "ww-official", 1000002)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "access_token": "tok", "expires_in": 7200})
		case "/cgi-bin/get_jsapi_ticket":
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "ticket": "jt", "expires_in": 7200})
		case "/cgi-bin/ticket/get":
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "ticket": "at", "expires_in": 7200})
		case "/cgi-bin/user/getuserinfo":
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "userid": "hongli"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	gw, err := NewOfficialGateway(config.Config{WeComProvider: "official", WeComTenantCfg: cfgPath})
	require.NoError(t, err)
	gw.baseURL = srv.URL
	gw.httpc = srv.Client()

	b, err := NewOfficialJSAPIConfigBuilder(gw)
	require.NoError(t, err)
	cfg, err := b.BuildJSAPIConfig(context.Background(), "https://wecom.changan.example.com/assistant")
	require.NoError(t, err)
	require.Equal(t, "ww-official", cfg.CorpID)
	require.Equal(t, "1000002", cfg.AgentID)
	require.Len(t, cfg.Signature, 40)
	require.Len(t, cfg.AgentSignature, 40)

	store := &fakeBindingStore{tenant: "chang-an", binding: []byte(`{"customer_id":"cust-0001","owner_user_id":"u-zhang","owner_name":"张销售"}`)}
	ip := NewOfficialIdentityProvider(gw, store)
	ex, err := ip.Exchange(context.Background(), "code-1", "chat-1")
	require.NoError(t, err)
	require.Equal(t, "hongli", ex.PrincipalID)
	require.Equal(t, "chang-an", ex.TenantID)
	require.Equal(t, "cust-0001", ex.CustomerID)
	require.True(t, ex.CustomerManual)

	// 无 chat 上下文 → 仅身份
	ex2, err := ip.Exchange(context.Background(), "code-1", "")
	require.NoError(t, err)
	require.Equal(t, "hongli", ex2.PrincipalID)
	require.Equal(t, "", ex2.CustomerID)

	// 未绑定群 / 绑定缺少 customer_id → 明确错误
	nf := &fakeBindingStore{err: errors.New("not found")}
	_, err = NewOfficialIdentityProvider(gw, nf).Exchange(context.Background(), "code-1", "chat-x")
	require.Error(t, err)
	bad := &fakeBindingStore{tenant: "chang-an", binding: []byte(`{"owner":"u-zhang"}`)}
	_, err = NewOfficialIdentityProvider(gw, bad).Exchange(context.Background(), "code-1", "chat-x")
	require.Error(t, err)
	require.Contains(t, err.Error(), "customer_id")
}
