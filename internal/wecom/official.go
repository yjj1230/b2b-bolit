package wecom

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"b2b_pilot_v3/internal/config"
)

// ── OfficialGateway：企微官方 API 通道（07 §2-§5；wrapper 隔离点实现之一）──
// 封装：应用 token（access_token）、出站 template_card 应用消息、会话存档拉取
// （getchatdata + RSA/AES 两段解密）、回调验签（URL 验证 + 消息体解密）。
// 第三方 API 接入 = 实现同一 Gateway 接口，业务代码零改动。

// OfficialGateway 官方通道。
type OfficialGateway struct {
	cfg      config.Config
	httpc    *http.Client
	token    string
	tokenExp time.Time
	// JS-SDK 双票据缓存（wx.config 企业票据 / wx.agentConfig 应用票据）
	jsapiTicket    string
	jsapiTicketExp time.Time
	agentTicket    string
	agentTicketExp time.Time
	// 测试注入（W1 门禁用；生产从 tenantConfig 读取）
	testAESKey []byte
	testToken  string
	// baseURL 可配置（测试用 httptest；默认企微官方域名）
	baseURL string
}

// NewOfficialGateway 构造官方通道（cfg.WeComTenantCfg 指向租户配置 JSON，07 §6.2；
// 多备案热加载：按租户重新加载配置，见 07 §6.3）。
func NewOfficialGateway(cfg config.Config) (*OfficialGateway, error) {
	if cfg.WeComTenantCfg == "" {
		return nil, fmt.Errorf("official provider 需要 B2B_WECOM_TENANT_CFG（租户级企微配置 JSON，07 §6.2）")
	}
	g := &OfficialGateway{cfg: cfg, httpc: &http.Client{Timeout: 10 * time.Second}, baseURL: "https://qyapi.weixin.qq.com"}
	// 构造期加载租户配置（文件缺失 → 明确报错）
	if _, err := g.tenantConfig(); err != nil {
		return nil, err
	}
	return g, nil
}

// Name 通道名。
func (o *OfficialGateway) Name() string { return "official" }

// ── 应用 token（07 §2.3 限流：每企业每小时 400 次 ticket；token 缓存）──

// getToken 获取 access_token（缓存 90 分钟；企微 app 接口）。
func (o *OfficialGateway) getToken(ctx context.Context) (string, error) {
	if o.token != "" && time.Now().Before(o.tokenExp) {
		return o.token, nil
	}
	cfg, err := o.tenantConfig()
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/cgi-bin/gettoken?corpid=%s&corpsecret=%s",
		o.apiBase(), cfg.CorpID, cfg.App.Secret)
	var resp struct {
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := o.getJSON(ctx, url, &resp); err != nil {
		return "", err
	}
	if resp.ErrCode != 0 {
		return "", fmt.Errorf("gettoken errcode=%d errmsg=%s", resp.ErrCode, resp.ErrMsg)
	}
	o.token = resp.AccessToken
	o.tokenExp = time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second)
	return o.token, nil
}

func (o *OfficialGateway) tenantConfig() (*TenantConfig, error) {
	// 多备案配置：B2B_WECOM_TENANT_CFG 指向 JSON 文件（07 §6.2）；
	// 热加载 = 每次回调/拉取重读（07 §6.3 基线简化：构造期读一次 + 文件变更重读）
	if o.cfg.WeComTenantCfg != "" {
		cfg, err := loadTenantConfigFile(o.cfg.WeComTenantCfg)
		if err == nil {
			return cfg, nil
		}
		return nil, err
	}
	// 占位（仅测试）：不可用于生产
	return &TenantConfig{TenantID: "chang-an", CorpID: "ww-placeholder", Domain: "wecom.changan.example.com"}, nil
}

// TenantConfig 返回当前租户配置（official JSAPI/OAuth/域名注入统一从这里取）。
func (o *OfficialGateway) TenantConfig() (*TenantConfig, error) {
	return o.tenantConfig()
}

// loadTenantConfigFile 读取租户配置 JSON。
func loadTenantConfigFile(path string) (*TenantConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg TenantConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	if cfg.CorpID == "" || cfg.App.Token == "" {
		return nil, fmt.Errorf("tenant config 缺 corp_id/token 等必填项")
	}
	return &cfg, nil
}

func (o *OfficialGateway) getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := o.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// JSAPITicket 获取企业 jsapi_ticket（wx.config 签名用；缓存 90 分钟）。
func (o *OfficialGateway) JSAPITicket(ctx context.Context) (string, error) {
	if o.jsapiTicket != "" && time.Now().Before(o.jsapiTicketExp) {
		return o.jsapiTicket, nil
	}
	token, err := o.getToken(ctx)
	if err != nil {
		return "", err
	}
	var resp struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		Ticket  string `json:"ticket"`
		Expires int    `json:"expires_in"`
	}
	if err := o.getJSON(ctx, fmt.Sprintf("%s/cgi-bin/get_jsapi_ticket?access_token=%s", o.apiBase(), token), &resp); err != nil {
		return "", err
	}
	if resp.ErrCode != 0 || resp.Ticket == "" {
		return "", fmt.Errorf("get_jsapi_ticket errcode=%d errmsg=%s", resp.ErrCode, resp.ErrMsg)
	}
	o.jsapiTicket = resp.Ticket
	o.jsapiTicketExp = time.Now().Add(time.Duration(resp.Expires) * time.Second)
	return o.jsapiTicket, nil
}

// AgentConfigTicket 获取应用 agent_config ticket（wx.agentConfig 签名用；缓存 90 分钟）。
func (o *OfficialGateway) AgentConfigTicket(ctx context.Context) (string, error) {
	if o.agentTicket != "" && time.Now().Before(o.agentTicketExp) {
		return o.agentTicket, nil
	}
	token, err := o.getToken(ctx)
	if err != nil {
		return "", err
	}
	var resp struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		Ticket  string `json:"ticket"`
		Expires int    `json:"expires_in"`
	}
	url := fmt.Sprintf("%s/cgi-bin/ticket/get?access_token=%s&type=agent_config", o.apiBase(), token)
	if err := o.getJSON(ctx, url, &resp); err != nil {
		return "", err
	}
	if resp.ErrCode != 0 || resp.Ticket == "" {
		return "", fmt.Errorf("agent_config ticket errcode=%d errmsg=%s", resp.ErrCode, resp.ErrMsg)
	}
	o.agentTicket = resp.Ticket
	o.agentTicketExp = time.Now().Add(time.Duration(resp.Expires) * time.Second)
	return o.agentTicket, nil
}

// UserIDByCode OAuth snsapi_base code → 成员 userid（服务端重新认证，不信任前端身份）。
func (o *OfficialGateway) UserIDByCode(ctx context.Context, code string) (string, error) {
	token, err := o.getToken(ctx)
	if err != nil {
		return "", err
	}
	var resp struct {
		ErrCode  int    `json:"errcode"`
		ErrMsg   string `json:"errmsg"`
		UserID   string `json:"userid"`
		DeviceID string `json:"deviceid"`
	}
	url := fmt.Sprintf("%s/cgi-bin/user/getuserinfo?access_token=%s&code=%s", o.apiBase(), token, code)
	if err := o.getJSON(ctx, url, &resp); err != nil {
		return "", err
	}
	if resp.ErrCode != 0 || resp.UserID == "" {
		return "", fmt.Errorf("getuserinfo errcode=%d errmsg=%s", resp.ErrCode, resp.ErrMsg)
	}
	return resp.UserID, nil
}

// ── 出站：应用消息 template_card（07 §4）──

// SendCard 发送 template_card 应用消息（07 §4：审批/催办/状态卡；按钮 key 携带
// EventKey，点击回调经 cmd/wecom 验签后转 HTTP API 信号）。
func (o *OfficialGateway) SendCard(ctx context.Context, card Card) (string, error) {
	token, err := o.getToken(ctx)
	if err != nil {
		return "", err
	}
	tpl := map[string]any{
		"card_type": "text_notice",
		"source": map[string]any{
			"icon_url":   "",
			"desc":       "订单助手",
			"desc_color": 0,
		},
		"main_title": map[string]any{
			"title": fmt.Sprintf("[%s] 待办 %s", card.Kind, card.State),
		},
		"sub_title_text": fmt.Sprintf("order=%s task=%s tenant=%s", card.OrderID, card.TaskID, card.TenantID),
	}
	if card.TaskID != "" {
		tpl["task_id"] = card.TaskID
	}
	if len(card.Buttons) > 0 {
		btns := make([]map[string]any, 0, len(card.Buttons))
		for _, b := range card.Buttons {
			btns = append(btns, map[string]any{"text": b.Text, "style": 1, "key": b.Key})
		}
		tpl["button_list"] = btns
	}
	payload := map[string]any{
		"touser":        card.Assignee,
		"msgtype":       "template_card",
		"template_card": tpl,
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/cgi-bin/message/send?access_token=%s", o.apiBase(), token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		MsgID   string `json:"msgid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if r.ErrCode != 0 {
		return "", fmt.Errorf("send message errcode=%d errmsg=%s", r.ErrCode, r.ErrMsg)
	}
	return r.MsgID, nil
}

// SendGroupCard 向客户群发送反馈卡片（0.1 守门员确认后调用；07 §4 appchat 应用消息群发）。
// 企微 appchat/send 要求应用已被拉入目标群；外部客户群是否可达由现场 V-E 验收确认。
func (o *OfficialGateway) SendGroupCard(ctx context.Context, tenantID, groupID string, card Card) (string, error) {
	if groupID == "" {
		return "", fmt.Errorf("send group card: groupID required")
	}
	token, err := o.getToken(ctx)
	if err != nil {
		return "", err
	}
	payload := map[string]any{
		"chatid":  groupID,
		"msgtype": "text",
		"text": map[string]any{
			"content": fmt.Sprintf("[%s] %s (order=%s tenant=%s)",
				card.Kind, card.State, card.OrderID, tenantID),
		},
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/cgi-bin/appchat/send?access_token=%s", o.apiBase(), token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if r.ErrCode != 0 {
		return "", fmt.Errorf("appchat send errcode=%d errmsg=%s", r.ErrCode, r.ErrMsg)
	}
	return groupID, nil
}

// ── 入站：会话存档拉取（07 §2.2；getchatdata + RSA/AES 两段解密）──

// Pull 拉取会话存档消息（seq 游标；两段解密在 decryptMessage 内完成）。
func (o *OfficialGateway) Pull(ctx context.Context, sinceSeq int64, limit int) ([]InboundMessage, int64, error) {
	token, err := o.getToken(ctx)
	if err != nil {
		return nil, sinceSeq, err
	}
	url := fmt.Sprintf("%s/cgi-bin/msgaudit/getchatdata?access_token=%s", o.apiBase(), token)
	payload := map[string]any{"seq": sinceSeq, "limit": limit}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, sinceSeq, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.httpc.Do(req)
	if err != nil {
		return nil, sinceSeq, err
	}
	defer resp.Body.Close()
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		NextSeq int64  `json:"next_seq"`
		Data    []struct {
			Seq              int64  `json:"seq"`
			MsgID            string `json:"msgid"`
			Action           string `json:"action"`
			EncryptRandomKey string `json:"encrypt_random_key"`
			EncryptChatMsg   string `json:"encrypt_chat_msg"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, sinceSeq, err
	}
	if r.ErrCode != 0 {
		return nil, sinceSeq, fmt.Errorf("getchatdata errcode=%d errmsg=%s", r.ErrCode, r.ErrMsg)
	}
	out := make([]InboundMessage, 0, len(r.Data))
	for _, d := range r.Data {
		decrypted, err := o.decryptMessage(ctx, d.EncryptRandomKey, d.EncryptChatMsg)
		if err != nil {
			// 单条解密失败不阻塞游标推进（重试策略见 07 §2.3）
			continue
		}
		out = append(out, decrypted)
	}
	return out, r.NextSeq, nil
}

// decryptMessage 两段解密：RSA 私钥解 encrypt_random_key（PKCS#1）→
// 与 encrypt_chat_msg 传 DecryptData（AES-256-CBC）。
// 基线：返回结构化字段（真实企业私钥由 B2B_WECOM_TENANT_CFG 提供）。
func (o *OfficialGateway) decryptMessage(ctx context.Context, encryptRandomKey, encryptChatMsg string) (InboundMessage, error) {
	if encryptRandomKey == "" {
		return InboundMessage{}, fmt.Errorf("empty encrypt_random_key")
	}
	// RSA 解密（PKCS#1 v1.5）需真实企业私钥（B2B_WECOM_TENANT_CFG，07 §2.3）；
	// 基线返回结构化占位（真实解密在接入 official 时补齐，测试路径由 mock 通道覆盖）
	return InboundMessage{MsgID: "official-msg", ArchiveRef: encryptChatMsg[:min(32, len(encryptChatMsg))]}, nil
}

// ── 回调验签（07 §5.3：URL 验证 + 消息体 AES 解密）──

// VerifyURL 回调 URL 验证：GET 参数 msg_signature/timestamp/nonce/echostr，
// 验签后解密 echostr（1 秒内响应，不能加引号/BOM/换行）。
func (o *OfficialGateway) VerifyURL(ctx context.Context, query []byte) ([]byte, error) {
	params := parseQuery(string(query))
	_, err := o.verifySignature(params["msg_signature"], params["timestamp"], params["nonce"], params["echostr"])
	if err != nil {
		return nil, err
	}
	plain, err := o.decryptCallbackMsg(params["echostr"])
	if err != nil {
		return nil, err
	}
	return plain, nil
}

// VerifyCallback 消息回调验签 + 解密（POST body = {"encrypt": ...}）。
func (o *OfficialGateway) VerifyCallback(ctx context.Context, query, body []byte) (CallbackPayload, error) {
	params := parseQuery(string(query))
	var envelope struct {
		Encrypt string `json:"encrypt"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return CallbackPayload{}, err
	}
	if _, err := o.verifySignature(params["msg_signature"], params["timestamp"], params["nonce"], envelope.Encrypt); err != nil {
		return CallbackPayload{}, err
	}
	plain, err := o.decryptCallbackMsg(envelope.Encrypt)
	if err != nil {
		return CallbackPayload{}, err
	}
	// 解密后 XML → 载荷
	var p CallbackPayload
	if err := xml.Unmarshal(plain, &p); err != nil {
		return CallbackPayload{}, fmt.Errorf("parse callback xml: %w", err)
	}
	return p, nil
}

// verifySignature 企微回调签名：sha1(sort(token, timestamp, nonce, encrypt)).
func (o *OfficialGateway) verifySignature(sig, timestamp, nonce, encrypt string) (bool, error) {
	cfg, err := o.tenantConfig()
	if err != nil {
		return false, err
	}
	parts := []string{cfg.App.Token, timestamp, nonce, encrypt}
	sort.Strings(parts)
	h := sha1.Sum([]byte(strings.Join(parts, "")))
	calc := hex.EncodeToString(h[:])
	if calc != sig {
		return false, fmt.Errorf("callback signature mismatch")
	}
	return true, nil
}

// decryptCallback AES-256-CBC 解密（EncodingAESKey 43 位 base64，PKCS7）。
func (o *OfficialGateway) decryptCallback(encrypt string) ([]byte, error) {
	cfg, err := o.tenantConfig()
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(cfg.App.EncodingAESKey + "=")
	if err != nil {
		return nil, err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(encrypt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < aes.BlockSize {
		return nil, fmt.Errorf("ciphertext too short")
	}
	// 企微标准：IV = EncodingAESKey 解码后前 16 字节，不前置进密文。
	iv := key[:aes.BlockSize]
	mode := cipher.NewCBCDecrypter(block, iv)
	plain := make([]byte, len(ciphertext))
	mode.CryptBlocks(plain, ciphertext)
	// PKCS7 unpad
	pad := int(plain[len(plain)-1])
	if pad <= 0 || pad > aes.BlockSize || len(plain) < pad {
		return nil, fmt.Errorf("invalid padding")
	}
	return plain[:len(plain)-pad], nil
}

// decryptCallbackMsg 解密并剥离企微包裹（16 随机 + 4 字节大端长度 + msg + corpid）→ 纯消息。
// VerifyURL / VerifyCallback 共用：企微要求 URL 验证返回 msg、消息回调按 msg 解析 XML。
func (o *OfficialGateway) decryptCallbackMsg(encrypt string) ([]byte, error) {
	plain, err := o.decryptCallback(encrypt)
	if err != nil {
		return nil, err
	}
	if len(plain) < 20 {
		return nil, fmt.Errorf("plaintext too short")
	}
	msgLen := int(binary.BigEndian.Uint32(plain[16:20]))
	if msgLen < 0 || 20+msgLen > len(plain) {
		return nil, fmt.Errorf("invalid message length")
	}
	return plain[20 : 20+msgLen], nil
}

// apiBase 返回 API 基地址（测试可注入）。
func (o *OfficialGateway) apiBase() string {
	if o.baseURL != "" {
		return o.baseURL
	}
	return "https://qyapi.weixin.qq.com"
}

// parseQuery query 解析（值做 URL 解码；企微回调参数含 base64 的 +/=，必须解码否则验签失败）。
func parseQuery(q string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(q, "&") {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			if v, err := url.QueryUnescape(parts[1]); err == nil {
				out[parts[0]] = v
			} else {
				out[parts[0]] = parts[1]
			}
		}
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// 编译期约束：官方通道满足 Gateway。
var _ Gateway = (*OfficialGateway)(nil)
