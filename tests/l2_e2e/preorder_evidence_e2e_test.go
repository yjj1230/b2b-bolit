//go:build integration

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/tests/testkit"
)

// TestE2EPreOrderChain L2-12 · 预订单全链：消息 → 预订单 → 确认 → 派生正式订单（CQ-11）。
func TestE2EPreOrderChain(t *testing.T) {
	e := testkit.NewEnv(t)

	// 消息 → 预订单（msgid 幂等）
	rec := e.DoJSON("POST", "/api/v1/preorders", `{"group_id":"group-demo","msg_id":"e2e-msg-1","intent_version":"v1",
		"candidates":[{"product_sku":"SKU-C68","product_id":"prod-c68","quantity":10,"unit_price_cents":1000,"confidence":0.95}]}`,
		http.StatusOK, nil)
	var pre struct {
		PreOrderID string `json:"pre_order_id"`
		State      string `json:"state"`
		Created    bool   `json:"created"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pre))
	require.True(t, pre.Created)
	require.Equal(t, domain.StatePreCaptured, pre.State)

	// 同 msgid 重放 → 返回既有（消息幂等，CQ-11）
	rec2 := e.DoJSON("POST", "/api/v1/preorders", `{"group_id":"group-demo","msg_id":"e2e-msg-1","intent_version":"v1","candidates":[]}`,
		http.StatusOK, nil)
	var pre2 struct {
		PreOrderID string `json:"pre_order_id"`
		Created    bool   `json:"created"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &pre2))
	require.False(t, pre2.Created)
	require.Equal(t, pre.PreOrderID, pre2.PreOrderID)

	// 确认信号（人工闸门；AI 安全边界：确认才派生正式订单）
	recConf := e.DoJSON("POST", fmt.Sprintf("/api/v1/preorders/%s/signal/%s", pre.PreOrderID, "preorder-confirm"),
		`{"confirmed":true,"operator":"u-sales"}`, http.StatusOK, map[string]string{"X-User-Id": "dave-sales"})
	require.Equal(t, http.StatusOK, recConf.Code)

	// 轮询：预订单 → PRE_ORDER_REQUESTED，且 orders 表出现派生订单（订单幂等键）
	require.Eventually(t, func() bool {
		var state string
		if err := e.Conn().QueryRow(t.Context(),
			`SELECT state FROM pre_orders WHERE id=$1`, pre.PreOrderID).Scan(&state); err != nil {
			return false
		}
		return state == domain.StatePreOrderRequested
	}, 10e9, 50e6)

	var n int
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT COUNT(*) FROM orders WHERE tenant_id='chang-an' AND idempotency_key LIKE 'order:chang-an:pre:%'`).Scan(&n))
	require.Equal(t, 1, n)

	// 派生订单为款到、数量 10（确定性校验结果）
	var qty int64
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT quantity FROM order_items WHERE tenant_id='chang-an' AND order_id IN
		 (SELECT id FROM orders WHERE idempotency_key LIKE 'order:chang-an:pre:%')`).Scan(&qty))
	require.Equal(t, int64(10), qty)
}

// TestE2EPreOrderIgnore 预订单忽略：IGNORED 终态，不派生订单。
func TestE2EPreOrderIgnore(t *testing.T) {
	e := testkit.NewEnv(t)

	rec := e.DoJSON("POST", "/api/v1/preorders", `{"group_id":"group-demo","msg_id":"e2e-msg-2","intent_version":"v1",
		"candidates":[{"product_sku":"SKU-C68","product_id":"prod-c68","quantity":10,"unit_price_cents":1000,"confidence":0.95}]}`,
		http.StatusOK, nil)
	var pre struct {
		PreOrderID string `json:"pre_order_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pre))

	e.DoJSON("POST", fmt.Sprintf("/api/v1/preorders/%s/signal/%s", pre.PreOrderID, "preorder-ignore"),
		`{"ignored":true,"operator":"u-sales"}`, http.StatusOK, map[string]string{"X-User-Id": "dave-sales"})

	require.Eventually(t, func() bool {
		var state string
		if err := e.Conn().QueryRow(t.Context(),
			`SELECT state FROM pre_orders WHERE id=$1`, pre.PreOrderID).Scan(&state); err != nil {
			return false
		}
		return state == domain.StatePreIgnored
	}, 10e9, 50e6)

	var n int
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT COUNT(*) FROM orders WHERE tenant_id='chang-an' AND idempotency_key LIKE 'order:chang-an:pre:%'`).Scan(&n))
	require.Zero(t, n)
}

// TestE2EEvidenceChain L2-15 · 凭证链经 HTTP：上传追加版本/回放/权限拒绝（CQ-15）。
func TestE2EEvidenceChain(t *testing.T) {
	e := testkit.NewEnv(t)
	orderID := e.CreateOrder("款到", "order:chang-an:quick:e2e-evidence-1")

	// 推进到发货，产生一张发货单
	e.WaitOrderState(orderID, domain.StateORDPriceReviewing)
	e.Signal(orderID, sigPriceReview, `{"verdict":"APPROVED","operator":"u-supervisor"}`)
	e.WaitOrderState(orderID, domain.StateSTKStockReviewing)
	e.Signal(orderID, sigStockReview, `{"verdict":"APPROVED","operator":"u-op","warehouse_id":"wh-yangyuan"}`)
	e.WaitOrderState(orderID, domain.StateSTKWarehouseConfirm)
	e.Signal(orderID, sigWarehouseConfirm, `{"verdict":"APPROVED","operator":"u-wh-yangyuan"}`)
	e.WaitOrderState(orderID, domain.StateSTKPaymentConfirming)
	e.Signal(orderID, sigPaymentConfirm, `{"verdict":"APPROVED","operator":"u-accounting"}`)
	e.WaitOrderState(orderID, domain.StateSHPShipAssigning)
	e.Signal(orderID, sigShipAction, `{"verdict":"SHIP","operator":"u-wh-yangyuan","qty":40}`)
	e.WaitOrderState(orderID, domain.StateSHPPartialShipped)

	var shipmentID string
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT id FROM shipments WHERE tenant_id='chang-an' AND order_id=$1 LIMIT 1`, orderID).Scan(&shipmentID))

	// 上传 v1/v2（仓管可上传：shipment#warehouse_operator 场景 C/D）
	path := fmt.Sprintf("/api/v1/orders/%s/shipments/%s/evidence", orderID, shipmentID)
	rec := e.DoJSON("POST", path, `{"file_url":"s3://e/v1.png","file_hash":"h1"}`, http.StatusCreated,
		map[string]string{"X-User-Id": "u-wh-yangyuan"})
	var out struct {
		Version int `json:"version"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, 1, out.Version)

	rec = e.DoJSON("POST", path, `{"file_url":"s3://e/v2.png","file_hash":"h2"}`, http.StatusCreated,
		map[string]string{"X-User-Id": "u-wh-yangyuan"})
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, 2, out.Version)

	// 回放：全版本有序（CQ-15 任一版本可回放）
	rec = e.DoJSON("GET", path, "", http.StatusOK, map[string]string{"X-User-Id": "u-wh-yangyuan"})
	var list struct {
		Versions []struct {
			Version int `json:"version"`
		} `json:"versions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Versions, 2)
	require.Equal(t, 1, list.Versions[0].Version)
	require.Equal(t, 2, list.Versions[1].Version)

	// 权限拒绝：客户联系人无 append_evidence（L2-15；场景 D 权限边界）
	rec = e.Do("POST", path, `{"file_url":"s3://e/v3.png","file_hash":"h3"}`, map[string]string{"X-User-Id": "u-contact"})
	require.Equal(t, http.StatusForbidden, rec.Code)

	// 参数校验：缺 file_url/file_hash → 400
	rec = e.Do("POST", path, `{"file_url":""}`, map[string]string{"X-User-Id": "u-wh-yangyuan"})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// 不存在的发货单 → 404
	rec = e.Do("POST", fmt.Sprintf("/api/v1/orders/%s/shipments/SHP-nope/evidence", orderID),
		`{"file_url":"s3://e/x.png","file_hash":"h"}`, map[string]string{"X-User-Id": "u-wh-yangyuan"})
	require.Equal(t, http.StatusNotFound, rec.Code)

	// 只追加：DB 侧无 UPDATE/DELETE 路径，版本 1 的 file_hash 未被覆盖
	var hash string
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT file_hash FROM evidence_versions WHERE tenant_id='chang-an' AND shipment_id=$1 AND version=1`, shipmentID).Scan(&hash))
	require.Equal(t, "h1", hash)
}

// TestE2EProjection L1-10/L2 · BFF 字段投影：仓管剥离价格；商务保留（CQ-03）。
func TestE2EProjection(t *testing.T) {
	e := testkit.NewEnv(t)
	orderID := e.CreateOrder("款到", "order:chang-an:quick:e2e-proj-1")

	// 仓管视图：无价格/金额字段
	rec := e.DoJSON("GET", "/api/v1/orders/"+orderID, "", http.StatusOK, map[string]string{"X-User-Id": "u-wh-yangyuan"})
	var wh struct {
		UnitPriceCents *int64 `json:"unit_price_cents"`
		AmountCents    *int64 `json:"amount_cents"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &wh))
	require.Nil(t, wh.UnitPriceCents, "warehouse must not see price")
	require.Nil(t, wh.AmountCents)

	// 商务视图：保留价格/金额
	rec = e.DoJSON("GET", "/api/v1/orders/"+orderID, "", http.StatusOK, map[string]string{"X-User-Id": "u-op"})
	var op struct {
		UnitPriceCents *int64 `json:"unit_price_cents"`
		AmountCents    *int64 `json:"amount_cents"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &op))
	require.NotNil(t, op.UnitPriceCents)
	require.NotNil(t, op.AmountCents)
	require.Equal(t, int64(1000), *op.UnitPriceCents)
	require.Equal(t, int64(100000), *op.AmountCents)
}

// TestE2EAuthzDenied L2-14 · 授权拒绝：无权限提交/审批 → 403（独立验证者拦截）。
func TestE2EAuthzDenied(t *testing.T) {
	e := testkit.NewEnv(t)

	// 无权限用户（未知用户默认 OP，可下单）——下单成功但…（StaticChecker 是替身，此处验证 API 层错误路径）
	orderID := e.CreateOrder("款到", "order:chang-an:quick:e2e-deny-1")

	// AUTH-01：信号级角色闸门——仓管投递审价信号 → 403（不再依赖工作流内判定）
	rec := e.Do("POST", fmt.Sprintf("/api/v1/orders/%s/signal/price-review", orderID),
		`{"verdict":"APPROVED","operator":"u-supervisor"}`, map[string]string{"X-User-Id": "u-wh-dongguan"})
	require.Equal(t, http.StatusForbidden, rec.Code)

	// 错误订单查询 → 404（跨对象不可见）
	rec = e.Do("GET", "/api/v1/orders/ORD-nope", "", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}
