package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/wecom"
	"b2b_pilot_v3/internal/workflows"
)

// ── 0.1 发布版端点测试（docs/v0.1/09-testing §3 用例 N-01/N-06/N-08）──
// HTTP 表面 + FakeStore 语义：守门员确认幂等/发送时机、凭证金额守恒、仓管金额不可见。

// newV01Test 构造带守门员通道与销售角色解析的服务。
func newV01Test(t *testing.T) (*Server, *workflows.FakeStore, *wecom.MockGateway) {
	t.Helper()
	store := workflows.NewFakeStoreForTest()
	starter := newFakeStarter(store)
	roleOf := func(userID string) (authz.Role, string) {
		switch userID {
		case "frank-wh":
			return authz.RoleWarehouse, "chang-an"
		case "carol-sales":
			return authz.RoleSales, "chang-an"
		default:
			return authz.RoleOP, "chang-an"
		}
	}
	checker := authz.NewStaticChecker(func(userID string) (authz.Role, error) {
		r, _ := roleOf(userID)
		return r, nil
	})
	gw := wecom.NewMockGateway()
	srv := NewServer(store, starter, checker, roleOf)
	srv.GroupSender = gw
	seedAPIWorld(t, store)
	return srv, store, gw
}

// N-01 · 发群守门员（A3）：未确认不发送；确认后发送并记录 GatekeeperConfirmed；
// 重复确认幂等（不重复发送）；非销售角色 403。
func TestN01GatekeeperSendToGroup(t *testing.T) {
	srv, _, gw := newV01Test(t)
	h := srv.Routes()

	// 下单（含收货地址 A1）
	rec := doJSON(t, h, "POST", "/api/v1/orders", `{"customer_id":"cust-0001","payment_term":"款到",
		"idempotency_key":"order:chang-an:gk:n01",
		"items":[{"id":"item-1","product_id":"prod-c68","warehouse_id":"wh-yangyuan","quantity":100,
		          "unit_price_cents":1000,"receive_address":"湖南岳阳 · 云溪区"}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created struct {
		OrderID string `json:"order_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	path := fmt.Sprintf("/api/v1/orders/%s/send-to-group", created.OrderID)
	headers := map[string]string{"X-User-Id": "carol-sales"}
	body := `{"group_id":"group-demo","kind":"order_card","content":"订单卡片 ORD 已发货","confirm":false}`

	// 1) 未确认：不发送、不记录事件
	rec1 := doJSON(t, h, "POST", path, body, headers)
	require.Equal(t, http.StatusOK, rec1.Code, rec1.Body.String())
	var r1 struct {
		OK                   bool `json:"ok"`
		ConfirmationRequired bool `json:"confirmation_required"`
	}
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &r1))
	require.False(t, r1.OK)
	require.True(t, r1.ConfirmationRequired)
	require.Empty(t, gw.SentGroupCards(), "未确认不得发送")

	// 2) 确认：发送 + GatekeeperConfirmed 事件
	rec2 := doJSON(t, h, "POST", path, `{"group_id":"group-demo","kind":"order_card","content":"订单卡片 ORD 已发货","confirm":true}`, headers)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	var r2 struct {
		OK               bool  `json:"ok"`
		Sent             bool  `json:"sent"`
		AlreadyConfirmed bool  `json:"already_confirmed"`
		EventID          int64 `json:"event_id"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &r2))
	require.True(t, r2.OK)
	require.True(t, r2.Sent)
	require.False(t, r2.AlreadyConfirmed)
	require.Equal(t, 1, len(gw.SentGroupCards()))

	// 3) 重复确认：幂等回执，不重复发送
	rec3 := doJSON(t, h, "POST", path, `{"group_id":"group-demo","kind":"order_card","content":"订单卡片 ORD 已发货","confirm":true}`, headers)
	require.Equal(t, http.StatusOK, rec3.Code, rec3.Body.String())
	var r3 struct {
		AlreadyConfirmed bool `json:"already_confirmed"`
		Sent             bool `json:"sent"`
	}
	require.NoError(t, json.Unmarshal(rec3.Body.Bytes(), &r3))
	require.True(t, r3.AlreadyConfirmed)
	require.False(t, r3.Sent)
	require.Equal(t, 1, len(gw.SentGroupCards()), "确认只发生一次")

	// 4) 不同内容：可再次确认（内容指纹区分）
	rec4 := doJSON(t, h, "POST", path, `{"group_id":"group-demo","kind":"logistics_image","content":"物流图片 德邦 DBL-1","confirm":true}`, headers)
	require.Equal(t, http.StatusOK, rec4.Code, rec4.Body.String())
	require.Equal(t, 2, len(gw.SentGroupCards()))

	// 5) 非销售角色：403（守门员唯一确认角色）
	rec5 := doJSON(t, h, "POST", path, `{"group_id":"group-demo","kind":"order_card","content":"x","confirm":true}`,
		map[string]string{"X-User-Id": "frank-wh"})
	require.Equal(t, http.StatusForbidden, rec5.Code, rec5.Body.String())

	// 6) 空内容：400
	rec6 := doJSON(t, h, "POST", path, `{"group_id":"g","content":"  ","confirm":true}`, headers)
	require.Equal(t, http.StatusBadRequest, rec6.Code, rec6.Body.String())

	// 7) 超长内容（>8KB）→ 400
	long := "{\"group_id\":\"g\",\"content\":\"" + strings.Repeat("x", 8193) + "\",\"confirm\":true}"
	rec7 := doJSON(t, h, "POST", path, long, headers)
	require.Equal(t, http.StatusBadRequest, rec7.Code, rec7.Body.String())

	// 8) 非法 body → 400
	rec8 := doJSON(t, h, "POST", path, `{bad`, headers)
	require.Equal(t, http.StatusBadRequest, rec8.Code, rec8.Body.String())

	// 9) 未知订单 → 404
	rec9 := doJSON(t, h, "POST", "/api/v1/orders/ORD-nope/send-to-group",
		`{"group_id":"g","content":"x","confirm":true}`, headers)
	require.Equal(t, http.StatusNotFound, rec9.Code, rec9.Body.String())
}

// N-06 · 凭证上传主路径 + 金额守恒（A2）：开票金额 = 已发货金额；超限拒绝；
// 凭证追加 v2 不覆盖 v1（只追加）。
func TestN06EvidenceAmountConservation(t *testing.T) {
	srv, store, _ := newV01Test(t)
	h := srv.Routes()

	orderID := createAPIOrder(t, h)
	store.SeedPrepared("chang-an", orderID, "item-1", 100)
	store.SeedShipment("chang-an", orderID, "SHP-n06", "wh-yangyuan", 100)
	shp := store.ShipmentFor("chang-an", "SHP-n06")
	shp.State = domain.ShipmentStateShipped
	shp.ShippedQty = 100
	shp.PlannedQty = 100

	path := fmt.Sprintf("/api/v1/orders/%s/shipments/SHP-n06/evidence", orderID)

	// 1) 金额 = 已发货金额（100 qty × 1000 分 = 100000）→ 201 v1
	rec1 := doJSON(t, h, "POST", path, `{"file_url":"file://invoice1.pdf","file_hash":"h1",
		"kind":"INVOICE","source":"manual","invoice_no":"NO 04511230","amount_cents":100000,"tax_cents":13000}`, nil)
	require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())
	var v1 struct {
		Version int `json:"version"`
	}
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &v1))
	require.Equal(t, 1, v1.Version)

	// 2) 超限（100001 > 100000）→ 409 拒绝
	rec2 := doJSON(t, h, "POST", path, `{"file_url":"file://invoice2.pdf","file_hash":"h2",
		"kind":"INVOICE","invoice_no":"NO 04511230-2","amount_cents":100001}`, nil)
	require.Equal(t, http.StatusConflict, rec2.Code, rec2.Body.String())

	// 3) 追加 v2 不覆盖 v1（只追加）
	rec3 := doJSON(t, h, "POST", path, `{"file_url":"file://invoice2.pdf","file_hash":"h2",
		"kind":"INVOICE","source":"robot","invoice_no":"NO 04511230","amount_cents":100000}`, nil)
	require.Equal(t, http.StatusCreated, rec3.Code, rec3.Body.String())
	var v2 struct {
		Version int `json:"version"`
	}
	require.NoError(t, json.Unmarshal(rec3.Body.Bytes(), &v2))
	require.Equal(t, 2, v2.Version)

	recList := doJSON(t, h, "GET", path, "", nil)
	require.Equal(t, http.StatusOK, recList.Code, recList.Body.String())
	var list struct {
		Versions []repo.EvidenceVersion `json:"versions"`
	}
	require.NoError(t, json.Unmarshal(recList.Body.Bytes(), &list))
	require.Len(t, list.Versions, 2, "v1 保留（只追加不覆盖）")
	require.Equal(t, "NO 04511230", list.Versions[1].InvoiceNo)
	require.Equal(t, "robot", list.Versions[1].Source)
}

// N-08 · 仓管金额不可见（CQ-03）：仓管视图剥离价格/金额；收货地址仍可见。
func TestN08WarehouseAmountStripped(t *testing.T) {
	srv, _, _ := newV01Test(t)
	h := srv.Routes()

	rec := doJSON(t, h, "POST", "/api/v1/orders", `{"customer_id":"cust-0001","payment_term":"款到",
		"idempotency_key":"order:chang-an:wh:n08",
		"items":[{"id":"item-1","product_id":"prod-c68","warehouse_id":"wh-yangyuan","quantity":100,
		          "unit_price_cents":1000,"receive_address":"湖南岳阳 · 云溪区"}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created struct {
		OrderID string `json:"order_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	// 商务视图：金额字段存在
	recOp := doJSON(t, h, "GET", "/api/v1/orders/"+created.OrderID, "", map[string]string{"X-User-Id": "u-op"})
	require.Equal(t, http.StatusOK, recOp.Code, recOp.Body.String())
	var opView OrderView
	require.NoError(t, json.Unmarshal(recOp.Body.Bytes(), &opView))
	require.NotNil(t, opView.UnitPriceCents, "ops sees price")
	require.NotNil(t, opView.AmountCents, "ops sees amount")

	// 仓管视图：金额剥离，收货地址可见（备货核对要素）
	recWh := doJSON(t, h, "GET", "/api/v1/orders/"+created.OrderID, "", map[string]string{"X-User-Id": "frank-wh"})
	require.Equal(t, http.StatusOK, recWh.Code, recWh.Body.String())
	var whView OrderView
	require.NoError(t, json.Unmarshal(recWh.Body.Bytes(), &whView))
	require.Nil(t, whView.UnitPriceCents, "warehouse must not see unit price")
	require.Nil(t, whView.AmountCents, "warehouse must not see amount")
	require.NotEmpty(t, whView.Items)
	require.Equal(t, "湖南岳阳 · 云溪区", whView.Items[0].ReceiveAddress)
}

type failingGroupSender struct{}

func (failingGroupSender) SendGroupCard(ctx context.Context, tenantID, groupID string, card wecom.Card) (string, error) {
	return "", errors.New("appchat not reachable")
}

// WEC-04 收敛：官方群发失败不使守门员确认失败——返回可读回退指令（PC 兜底/人工转发）。
func TestN01GatekeeperSendFallback(t *testing.T) {
	srv, _, _ := newV01Test(t)
	srv.GroupSender = failingGroupSender{}
	h := srv.Routes()
	orderID := createAPIOrder(t, h)

	rec := doJSON(t, h, "POST", "/api/v1/orders/"+orderID+"/send-to-group",
		`{"group_id":"wr-cust","kind":"order_card","content":"订单卡片内容","confirm":true}`,
		map[string]string{"X-User-Id": "carol-sales"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"sent":false`)
	require.Contains(t, rec.Body.String(), "fallback_required")
	require.Contains(t, rec.Body.String(), "PC 兜底")
}
