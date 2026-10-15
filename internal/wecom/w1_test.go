package wecom

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/config"
)

// ── W1 门禁前置：回调验签与解密（07 §8.1；真实 AES-256-CBC 往返）──

// 构造带测试密钥的官方通道（绕过 tenantConfig 占位）。
func testOfficialGateway(t *testing.T) *OfficialGateway {
	t.Helper()
	o := &OfficialGateway{}
	// 生成确定性 32 字节 AES 密钥 + 43 位 EncodingAESKey（企微规范）
	rawKey := []byte("0123456789abcdef0123456789abcdef") // 32B
	aesKey := base64.StdEncoding.EncodeToString(rawKey)
	_ = aesKey
	o.testAESKey = rawKey
	o.testToken = "test-token-123"
	return o
}

// wecomEncrypt 企微回调加密（AES-256-CBC，随机 IV，PKCS7）。
func wecomEncrypt(t *testing.T, key []byte, plaintext []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	iv := make([]byte, aes.BlockSize)
	for i := range iv {
		iv[i] = byte(i)
	}
	padLen := aes.BlockSize - len(plaintext)%aes.BlockSize
	pad := make([]byte, padLen)
	for i := range pad {
		pad[i] = byte(padLen)
	}
	data := append(plaintext, pad...)
	out := make([]byte, len(iv)+len(data))
	copy(out, iv)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out[aes.BlockSize:], data)
	return base64.StdEncoding.EncodeToString(out)
}

// wecomEncryptEnvelope 企微协议包裹加密（16 随机 + 4 字节大端长度 + msg + corpid），
// 供走生产解密路径（decryptCallbackMsg / VerifyURL / VerifyCallback）的用例使用。
// 与 wecomEncrypt 的两点差异，均对齐生产实现（official.go decryptCallback）：
//  1. IV = key[:16]，且不前置进密文（企微标准；wecomEncrypt 走测试专用路径，IV 前置）；
//  2. 明文包裹 16 随机 + 4 字节大端 length + msg + corpid。
func wecomEncryptEnvelope(t *testing.T, key []byte, msg []byte, corpID string) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	randBytes := make([]byte, 16)
	for i := range randBytes {
		randBytes[i] = byte(i + 1)
	}
	var msgLen [4]byte
	binary.BigEndian.PutUint32(msgLen[:], uint32(len(msg)))
	buf := make([]byte, 0, 16+4+len(msg)+len(corpID))
	buf = append(buf, randBytes...)
	buf = append(buf, msgLen[:]...)
	buf = append(buf, msg...)
	buf = append(buf, []byte(corpID)...)
	padLen := aes.BlockSize - len(buf)%aes.BlockSize
	pad := make([]byte, padLen)
	for i := range pad {
		pad[i] = byte(padLen)
	}
	data := append(buf, pad...)
	out := make([]byte, len(data))
	cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(out, data)
	return base64.StdEncoding.EncodeToString(out)
}

func TestW1CallbackDecryptRoundTrip(t *testing.T) {
	o := testOfficialGateway(t)
	plain := `<xml><EventKey>approve:AT-1</EventKey><FromUserName>u-supervisor</FromUserName></xml>`
	enc := wecomEncrypt(t, o.testAESKey, []byte(plain))

	got, err := o.decryptCallbackWithKey(enc, o.testAESKey)
	require.NoError(t, err)
	require.Equal(t, plain, string(got))

	// 篡改密文（尾部翻转）→ 解密失败（padding 校验）
	tampered := enc[:len(enc)-10] + flipBase64(enc[len(enc)-10]) + enc[len(enc)-9:]
	_, err = o.decryptCallbackWithKey(tampered, o.testAESKey)
	require.Error(t, err)
}

func TestW1SignatureVerify(t *testing.T) {
	o := testOfficialGateway(t)
	parts := []string{o.testToken, "1700000000", "nonce1", "encrypted-body"}
	sort.Strings(parts)
	h := sha1.Sum([]byte(strings.Join(parts, "")))
	sig := hex.EncodeToString(h[:])

	ok, err := o.verifySignatureWithToken(sig, "1700000000", "nonce1", "encrypted-body", o.testToken)
	require.NoError(t, err)
	require.True(t, ok)

	// 篡改 → 拒绝
	ok, err = o.verifySignatureWithToken("deadbeef", "1700000000", "nonce1", "encrypted-body", o.testToken)
	require.Error(t, err)
	require.False(t, ok)
}

func TestW1VerifyCallbackAndURL(t *testing.T) {
	o := testOfficialGateway(t)
	ctx := context.Background()

	// URL 验证：GET echostr 解密
	echo := "1234567890abcdef"
	encEcho := wecomEncrypt(t, o.testAESKey, []byte(echo))
	query := buildQuery(t, o, encEcho, encEcho)
	got, err := o.VerifyURLWithToken(ctx, query, o.testToken, o.testAESKey)
	require.NoError(t, err)
	require.Equal(t, echo, string(got))

	// 消息回调：POST {"encrypt": ...} → 验签 + 解密 → 载荷
	payload := CallbackPayload{EventKey: "approve:AT-1", FromUserID: "u-supervisor"}
	bodyXML, _ := json.Marshal(payload)
	_ = bodyXML
	xmlBody := `<xml><EventKey>approve:AT-1</EventKey><FromUserName>u-supervisor</FromUserName></xml>`
	enc := wecomEncrypt(t, o.testAESKey, []byte(xmlBody))
	env, _ := json.Marshal(map[string]string{"encrypt": enc})
	q := buildQuery(t, o, "", enc)
	p, err := o.VerifyCallbackWithToken(ctx, q, env, o.testToken, o.testAESKey)
	require.NoError(t, err)
	require.Equal(t, "approve:AT-1", p.EventKey)
}

// WEC-02 收敛：官方群图片回调字段（ChatId/PicUrl/MediaId）必须能解析进 CallbackPayload。
func TestW1GroupImageCallbackFields(t *testing.T) {
	raw := `<xml><MsgType><![CDATA[image]]></MsgType><ChatId><![CDATA[wr-group-1]]></ChatId>` +
		`<PicUrl><![CDATA[https://wework.qpic.cn/wwpic/123456_recv]]></PicUrl>` +
		`<MediaId><![CDATA[MEDIA_ID_IMG_1]]></MediaId></xml>`
	var p CallbackPayload
	require.NoError(t, xml.Unmarshal([]byte(raw), &p))
	require.Equal(t, "image", p.MsgType)
	require.Equal(t, "wr-group-1", p.ChatID)
	require.Equal(t, "https://wework.qpic.cn/wwpic/123456_recv", p.ImageURL)
	require.Equal(t, "MEDIA_ID_IMG_1", p.MediaID)
}

func buildQuery(t *testing.T, o *OfficialGateway, echostr, encrypt string) []byte {
	t.Helper()
	parts := []string{o.testToken, "1700000000", "nonce1", encrypt}
	sort.Strings(parts)
	h := sha1.Sum([]byte(strings.Join(parts, "")))
	q := "msg_signature=" + hex.EncodeToString(h[:]) + "&timestamp=1700000000&nonce=nonce1"
	if echostr != "" {
		q += "&echostr=" + echostr
	}
	return []byte(q)
}

// decryptCallbackWithKey 直接注入密钥的解密（测试路径）。
func (o *OfficialGateway) decryptCallbackWithKey(encrypt string, key []byte) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encrypt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < aes.BlockSize {
		return nil, errCipherTooShort
	}
	iv := ciphertext[:aes.BlockSize]
	mode := cipher.NewCBCDecrypter(block, iv)
	plain := make([]byte, len(ciphertext)-aes.BlockSize)
	mode.CryptBlocks(plain, ciphertext[aes.BlockSize:])
	pad := int(plain[len(plain)-1])
	if pad <= 0 || pad > aes.BlockSize || len(plain) < pad {
		return nil, errBadPadding
	}
	return plain[:len(plain)-pad], nil
}

func (o *OfficialGateway) verifySignatureWithToken(sig, timestamp, nonce, encrypt, token string) (bool, error) {
	parts := []string{token, timestamp, nonce, encrypt}
	sort.Strings(parts)
	h := sha1.Sum([]byte(strings.Join(parts, "")))
	if hex.EncodeToString(h[:]) != sig {
		return false, errSigMismatch
	}
	return true, nil
}

func (o *OfficialGateway) VerifyURLWithToken(ctx context.Context, query []byte, token string, key []byte) ([]byte, error) {
	params := parseQuery(string(query))
	if _, err := o.verifySignatureWithToken(params["msg_signature"], params["timestamp"], params["nonce"], params["echostr"], token); err != nil {
		return nil, err
	}
	return o.decryptCallbackWithKey(params["echostr"], key)
}

func (o *OfficialGateway) VerifyCallbackWithToken(ctx context.Context, query, body []byte, token string, key []byte) (CallbackPayload, error) {
	params := parseQuery(string(query))
	var envelope struct {
		Encrypt string `json:"encrypt"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return CallbackPayload{}, err
	}
	if _, err := o.verifySignatureWithToken(params["msg_signature"], params["timestamp"], params["nonce"], envelope.Encrypt, token); err != nil {
		return CallbackPayload{}, err
	}
	plain, err := o.decryptCallbackWithKey(envelope.Encrypt, key)
	if err != nil {
		return CallbackPayload{}, err
	}
	var p CallbackPayload
	if err := xml.Unmarshal(plain, &p); err != nil {
		return CallbackPayload{}, err
	}
	return p, nil
}

var errCipherTooShort = errNew("ciphertext too short")
var errBadPadding = errNew("invalid padding")
var errSigMismatch = errNew("callback signature mismatch")

func errNew(msg string) error { return &simpleErr{msg} }

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }

// flipBase64 翻转一个 base64 字符（保持长度，制造密文篡改）。
func flipBase64(c byte) string {
	if c == 'A' {
		return "B"
	}
	return "A"
}

// 生产路径：租户配置文件 + 真实 AES 回调往返（VerifyURL/VerifyCallback/decryptCallback）
func TestOfficialProductionPathsWithTenantConfig(t *testing.T) {
	ctx := context.Background()
	key := []byte("0123456789abcdef0123456789abcdef")
	aesKeyB64 := strings.TrimRight(base64.StdEncoding.EncodeToString(key), "=") // 企微 43 位无填充
	cfgPath := writeTenantConfig(t, aesKeyB64)
	cfg := config.Config{WeComTenantCfg: cfgPath}
	o, err := NewOfficialGateway(cfg)
	require.NoError(t, err)

	// VerifyURL：签名 + echostr 解密
	echo := "verify-echo-123"
	encEcho := wecomEncryptEnvelope(t, key, []byte(echo), "ww-1")
	q := buildQueryWithToken("test-token", encEcho, encEcho)
	got, err := o.VerifyURL(ctx, q)
	require.NoError(t, err)
	require.Equal(t, echo, string(got))

	// VerifyCallback：签名 + body 解密 + 载荷
	xmlBody := `<xml><EventKey>preorder:confirm:PRE-1</EventKey><FromUserName>u-sales</FromUserName></xml>`
	enc := wecomEncryptEnvelope(t, key, []byte(xmlBody), "ww-1")
	env, _ := json.Marshal(map[string]string{"encrypt": enc})
	got2, err := o.VerifyCallback(ctx, buildQueryWithToken("test-token", "", enc), env)
	require.NoError(t, err)
	require.Equal(t, "preorder:confirm:PRE-1", got2.EventKey)
	require.Equal(t, "u-sales", got2.FromUserID)

	// 篡改 → 拒绝
	tampered := enc[:len(enc)-10] + flipBase64(enc[len(enc)-10]) + enc[len(enc)-9:]
	env2, _ := json.Marshal(map[string]string{"encrypt": tampered})
	_, err = o.VerifyCallback(ctx, buildQueryWithToken("test-token", "", tampered), env2)
	require.Error(t, err)

	// 配置文件缺失 → 明确报错
	_, err = NewOfficialGateway(config.Config{WeComTenantCfg: "/nonexistent/tenant.json"})
	require.Error(t, err)
	// 配置缺必填项
	bad := writeTenantConfigBad(t)
	_, err = NewOfficialGateway(config.Config{WeComTenantCfg: bad})
	require.Error(t, err)
}

func writeTenantConfig(t *testing.T, aesKeyB64 string) string {
	t.Helper()
	path := t.TempDir() + "/tenant.json"
	content := `{"tenant_id":"chang-an","corp_id":"ww-1","domain":"wecom.changan.example.com",
		"app":{"agent_id":1,"secret":"s","token":"test-token","encoding_aes_key":"` + aesKeyB64 + `"}}`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func writeTenantConfigBad(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/bad.json"
	require.NoError(t, os.WriteFile(path, []byte(`{"tenant_id":"x"}`), 0o600))
	return path
}

func buildQueryWithToken(token, echostr, encrypt string) []byte {
	parts := []string{token, "1700000000", "nonce1", encrypt}
	sort.Strings(parts)
	h := sha1.Sum([]byte(strings.Join(parts, "")))
	// 与真实企微回调一致：query 值做 URL 编码（base64 的 +/= 必须转义，
	// 否则 parseQuery 的 QueryUnescape 会把 + 还原成空格导致验签失配）。
	q := "msg_signature=" + hex.EncodeToString(h[:]) + "&timestamp=1700000000&nonce=nonce1"
	if echostr != "" {
		q += "&echostr=" + url.QueryEscape(echostr)
	}
	return []byte(q)
}
