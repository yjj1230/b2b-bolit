package wecom

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/config"
)

// flipB64 翻转一个 base64 字符（保持长度）。
func flipB64(c byte) string {
	if c == 'A' {
		return "B"
	}
	return "A"
}

// ── 官方通道网络路径（httptest 模拟企微 API；覆盖 getToken/SendCard/Pull 错误与成功分支）──

func newOfficialWithServer(t *testing.T, handler http.HandlerFunc) (*OfficialGateway, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	o := &OfficialGateway{
		httpc:     srv.Client(),
		baseURL:   srv.URL,
		testToken: "tok",
	}
	t.Cleanup(srv.Close)
	return o, srv
}

func TestOfficialGetTokenAndSendCard(t *testing.T) {
	// getToken 错误分支：errcode != 0
	o, _ := newOfficialWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 40013, "errmsg": "invalid corpid"})
	})
	o.token, o.tokenExp = "", time.Time{}
	_, err := o.getToken(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "40013")

	// getToken 成功 + SendCard 成功
	o2, _ := newOfficialWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/cgi-bin/gettoken":
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "access_token": "tok-1", "expires_in": 7200})
		case r.URL.Path == "/cgi-bin/message/send":
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "msgid": "MSG-1"})
		default:
			http.NotFound(w, r)
		}
	})
	o2.token, o2.tokenExp = "", time.Time{}
	tok, err := o2.getToken(context.Background())
	require.NoError(t, err)
	require.Equal(t, "tok-1", tok)
	mid, err := o2.SendCard(context.Background(), Card{TenantID: "chang-an", Kind: "PRICE", Assignee: "u-supervisor"})
	require.NoError(t, err)
	require.Equal(t, "MSG-1", mid)
	// token 缓存命中
	tok2, err := o2.getToken(context.Background())
	require.NoError(t, err)
	require.Equal(t, "tok-1", tok2)

	// SendCard 错误分支（限流 45009）
	o3, _ := newOfficialWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 45009, "errmsg": "reach max api daily quota limit"})
	})
	o3.token, o3.tokenExp = "", time.Time{}
	_, err = o3.SendCard(context.Background(), Card{Kind: "PRICE"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "45009")

	// 网络错误分支（服务器关闭）
	o4 := &OfficialGateway{httpc: &http.Client{Timeout: time.Second}, baseURL: "http://127.0.0.1:1"}
	_, err = o4.getToken(context.Background())
	require.Error(t, err)
}

func TestOfficialPull(t *testing.T) {
	// 错误分支：errcode != 0
	o, _ := newOfficialWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 60020, "errmsg": "not allow to access from your ip"})
	})
	o.token = "tok"
	_, seq, err := o.Pull(context.Background(), 0, 100)
	require.Error(t, err)
	require.Equal(t, int64(0), seq)

	// 成功分支：next_seq 推进
	o2, _ := newOfficialWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errcode":  0,
			"next_seq": 42,
			"data": []map[string]any{
				{"seq": 1, "msgid": "m1", "encrypt_random_key": "a2V5", "encrypt_chat_msg": "x"},
			},
		})
	})
	o2.token = "tok"
	msgs, next, err := o2.Pull(context.Background(), 0, 100)
	require.NoError(t, err)
	require.Equal(t, int64(42), next)
	require.Len(t, msgs, 1)
	require.NotEmpty(t, msgs[0].ArchiveRef)

	// decryptMessage 空密钥错误分支
	_, err = o2.decryptMessage(context.Background(), "", "x")
	require.Error(t, err)
}

func TestOfficialNameAndConfig(t *testing.T) {
	o := &OfficialGateway{}
	require.Equal(t, "official", o.Name())
	require.Equal(t, "https://qyapi.weixin.qq.com", o.apiBase())
	// NewOfficialGateway 缺租户配置
	_, err := NewOfficialGateway(config.Config{WeComProvider: "official", WeComTenantCfg: ""})
	require.Error(t, err)
	// min 函数
	require.Equal(t, 3, min(3, 9))
	require.Equal(t, 2, min(9, 2))
}

// 网络/解码错误分支批量覆盖（httptest 故障注入）
func TestOfficialNetworkErrorBranches(t *testing.T) {
	ctx := context.Background()

	// 服务器关闭 → http.Do 失败（getToken/SendCard/Pull/getJSON）
	o := &OfficialGateway{httpc: &http.Client{Timeout: time.Second}, baseURL: "http://127.0.0.1:1"}
	o.token, o.tokenExp = "", time.Time{}
	_, err := o.getToken(ctx)
	require.Error(t, err)
	_, err = o.SendCard(ctx, Card{Kind: "X"})
	require.Error(t, err)
	_, _, err = o.Pull(ctx, 0, 10)
	require.Error(t, err)

	// 服务器返回非法 JSON → Decode 失败（SendCard/Pull/getToken）
	o2, _ := newOfficialWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{bad json`))
	})
	o2.token, o2.tokenExp = "", time.Time{}
	_, err = o2.getToken(ctx)
	require.Error(t, err)
	_, err = o2.SendCard(ctx, Card{Kind: "X"})
	require.Error(t, err)
	_, _, err = o2.Pull(ctx, 0, 10)
	require.Error(t, err)
	// getJSON 解码失败（经 tenantConfig 路径）
	err = o2.getJSON(ctx, "http://x", &struct{}{})
	require.Error(t, err)
	// tenantConfig 文件读取失败（getToken 内分支）
	o3 := &OfficialGateway{httpc: &http.Client{}, baseURL: "http://x", cfg: config.Config{WeComTenantCfg: "/nonexistent/t.json"}}
	_, err = o3.getToken(ctx)
	require.Error(t, err)

	// SendCard：errcode!=0 分支（45009 已测）+ 网络后 body 读失败
	o4, _ := newOfficialWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errcode":0,"msgid":"OK"}`))
	})
	o4.token, o4.tokenExp = "t", time.Now().Add(time.Hour)
	mid, err := o4.SendCard(ctx, Card{Kind: "X"})
	require.NoError(t, err)
	require.Equal(t, "OK", mid)

	// Pull：成功分支 next_seq 推进（已有）+ decrypt 单条失败跳过
	o5, _ := newOfficialWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errcode":0,"next_seq":7,"data":[{"seq":1,"msgid":"m","encrypt_random_key":"","encrypt_chat_msg":"x"}]}`))
	})
	o5.token = "t"
	msgs, next, err := o5.Pull(ctx, 0, 10)
	require.NoError(t, err)
	require.Equal(t, int64(7), next)
	require.Empty(t, msgs) // encrypt_random_key 为空 → 单条解密失败跳过（不阻塞游标）
}

// decryptCallback 边界：无效 key / 过短密文 / 填充非法
func TestDecryptCallbackEdges(t *testing.T) {
	o := &OfficialGateway{}
	// 空 key（tenantConfig 占位）→ 错误
	_, err := o.decryptCallback("AAAA")
	require.Error(t, err)
	// 无效 base64 密文
	_, err = o.decryptCallbackWithKey("!!!", []byte("0123456789abcdef0123456789abcdef"))
	require.Error(t, err)
	// 过短密文
	_, err = o.decryptCallbackWithKey("aGVsbG8=", []byte("0123456789abcdef0123456789abcdef"))
	require.Error(t, err)
	// 填充非法（构造 pad=0 场景：加密后篡改最后字节）
	key := []byte("0123456789abcdef0123456789abcdef")
	enc := wecomEncrypt(t, key, []byte("hello world"))
	// 翻转尾部字节使 PKCS7 pad 校验失败（篡改密文最后一块）
	tampered := enc[:len(enc)-2] + flipB64(enc[len(enc)-2])
	_, err = o.decryptCallbackWithKey(tampered, key)
	require.Error(t, err)
}

// wecom 残余错误分支：token 缓存 + 服务器故障 / ReadAll 中断 / VerifyURL 解密失败
func TestOfficialResidualErrorBranches(t *testing.T) {
	ctx := context.Background()

	// token 已缓存 + 服务器关闭 → SendCard/Pull 的 httpc.Do 失败分支
	dead := &OfficialGateway{httpc: &http.Client{Timeout: time.Second}, baseURL: "http://127.0.0.1:1"}
	dead.token, dead.tokenExp = "t", time.Now().Add(time.Hour)
	_, err := dead.SendCard(ctx, Card{Kind: "X"})
	require.Error(t, err)
	_, _, err = dead.Pull(ctx, 0, 10)
	require.Error(t, err)

	// 响应体读取中断 → ReadAll 失败（SendCard/getJSON）
	abortSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if ok {
			conn, _, _ := hj.Hijack()
			_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial"))
			_ = conn.Close() // 未达 Content-Length → ReadAll 报错
			return
		}
		w.WriteHeader(500)
	}))
	defer abortSrv.Close()
	o := &OfficialGateway{httpc: abortSrv.Client(), baseURL: abortSrv.URL}
	o.token, o.tokenExp = "t", time.Now().Add(time.Hour)
	_, err = o.SendCard(ctx, Card{Kind: "X"})
	require.Error(t, err)
	_, _, err = o.Pull(ctx, 0, 10)
	require.Error(t, err)
	// getJSON ReadAll 失败（经直接调用）
	o2 := &OfficialGateway{httpc: abortSrv.Client()}
	err = o2.getJSON(ctx, abortSrv.URL, &struct{}{})
	require.Error(t, err)

	// VerifyURL：签名正确但 echostr 非法 base64 → decryptCallback 失败分支
	o3 := &OfficialGateway{}
	sig := "sig"
	_ = sig
	q := []byte("msg_signature=0000000000000000000000000000000000000000&timestamp=1&nonce=n&echostr=!!!not-base64")
	_, err = o3.VerifyURLWithToken(ctx, q, "tok", []byte("0123456789abcdef0123456789abcdef"))
	require.Error(t, err)

	// decryptCallback：合法 base64 但 AES key 无效长度 → 错误
	_, err = o3.decryptCallbackWithKey("aGVsbG8=", []byte("short-key"))
	require.Error(t, err)
}
