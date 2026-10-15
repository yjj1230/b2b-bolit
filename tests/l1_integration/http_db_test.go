//go:build integration

// Package l1int —— L1 · HTTP API × 真实 PostgreSQL 组件集成（docs/03-testing.html L1）
//
// 范围：HTTP API 与真库的契约（幂等/跨租户隔离/事件链/凭证只追加），不依赖工作流
// 执行完整性（那是 L2 的职责）。底座与 L2 相同：真实 PG + LocalRunner + StaticChecker。
package l1int

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/tests/testkit"
)

// TestL1HTTPIdempotency L1-02 经 HTTP · 幂等唯一：同键重放返回既有订单，不重复建单。
func TestL1HTTPIdempotency(t *testing.T) {
	e := testkit.NewEnvWithOptions(t, testkit.Options{Starter: &testkit.NoopStarter{}})
	body := `{"customer_id":"cust-0001","payment_term":"款到","supplier_channel":"渠道A",
		"idempotency_key":"order:chang-an:quick:l1-idem-1",
		"items":[{"id":"item-1","product_id":"prod-c68","warehouse_id":"wh-yangyuan","quantity":100,"unit_price_cents":1000}]}`

	rec1 := e.DoJSON("POST", "/api/v1/orders", body, http.StatusOK, nil)
	var o1 struct {
		OrderID string `json:"order_id"`
		Created bool   `json:"created"`
	}
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &o1))
	require.True(t, o1.Created)

	rec2 := e.DoJSON("POST", "/api/v1/orders", body, http.StatusOK, nil)
	var o2 struct {
		OrderID string `json:"order_id"`
		Created bool   `json:"created"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &o2))
	require.False(t, o2.Created)
	require.Equal(t, o1.OrderID, o2.OrderID)

	// DB 唯一约束兜底（防线 3）：orders 表仅 1 行
	var n int
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT COUNT(*) FROM orders WHERE tenant_id='chang-an' AND idempotency_key=$1`,
		"order:chang-an:quick:l1-idem-1").Scan(&n))
	require.Equal(t, 1, n)
}

// TestL1HTTPCrossTenant L1-02 经 HTTP · 跨租户：同 idempotency_key 各自生效、互不可见（02 §5）。
func TestL1HTTPCrossTenant(t *testing.T) {
	e := testkit.NewEnvWithOptions(t, testkit.Options{Starter: &testkit.NoopStarter{}})
	// 播种 tenant-b（标准世界只含 chang-an）；customer_id/product_id 为 pgAPI record_id 外部引用，无需本地主数据
	require.NoError(t, e.Store.SeedTenant(t.Context(), "tenant-b", "租户B"))
	require.NoError(t, e.Store.SeedUser(t.Context(), "tenant-b", "u-op-b", "商务B", "OP", "wecom_b"))
	require.NoError(t, e.Store.SeedWarehouse(t.Context(), "tenant-b", "wh-b1", "仓B"))

	const key = "order:same:key:both-tenants"
	bodyA := fmt.Sprintf(`{"customer_id":"cust-0001","payment_term":"款到","supplier_channel":"渠道A",
		"idempotency_key":%q,"items":[{"id":"i-a","product_id":"prod-c68","warehouse_id":"wh-yangyuan","quantity":10,"unit_price_cents":1000}]}`, key)
	bodyB := fmt.Sprintf(`{"customer_id":"cust-b1","payment_term":"款到","supplier_channel":"渠道B",
		"idempotency_key":%q,"items":[{"id":"i-b","product_id":"prod-b1","warehouse_id":"wh-b1","quantity":20,"unit_price_cents":2000}]}`, key)

	recA := e.DoJSON("POST", "/api/v1/orders", bodyA, http.StatusOK, map[string]string{"X-User-Id": "u-op"})
	var ordA struct {
		OrderID string `json:"order_id"`
	}
	require.NoError(t, json.Unmarshal(recA.Body.Bytes(), &ordA))

	recB := e.DoJSON("POST", "/api/v1/orders", bodyB, http.StatusOK, map[string]string{"X-User-Id": "u-op-b"})
	var ordB struct {
		OrderID string `json:"order_id"`
	}
	require.NoError(t, json.Unmarshal(recB.Body.Bytes(), &ordB))

	require.NotEqual(t, ordA.OrderID, ordB.OrderID)

	// 跨租户互不可见：chang-an 查 tenant-b 订单 → 404；反向亦然
	e.DoJSON("GET", "/api/v1/orders/"+ordB.OrderID, "", http.StatusNotFound, map[string]string{"X-User-Id": "u-op"})
	e.DoJSON("GET", "/api/v1/orders/"+ordA.OrderID, "", http.StatusNotFound, map[string]string{"X-User-Id": "u-op-b"})

	// 各自可见
	e.DoJSON("GET", "/api/v1/orders/"+ordA.OrderID, "", http.StatusOK, map[string]string{"X-User-Id": "u-op"})
	e.DoJSON("GET", "/api/v1/orders/"+ordB.OrderID, "", http.StatusOK, map[string]string{"X-User-Id": "u-op-b"})
}

// TestL1HTTPEventChain L1-12 经 HTTP · 分域事件链：创建即产生事件，只追加不覆盖（CQ-12）。
func TestL1HTTPEventChain(t *testing.T) {
	e := testkit.NewEnvWithOptions(t, testkit.Options{Starter: &testkit.NoopStarter{}})
	orderID := e.CreateOrder("款到", "order:chang-an:quick:l1-events-1")

	// 事件可经 HTTP 回放且按序（创建事件必然存在）
	rec := e.DoJSON("GET", "/api/v1/orders/"+orderID+"/events", "", http.StatusOK, nil)
	var out struct {
		Events []repo.Event `json:"events"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.NotEmpty(t, out.Events)
	found := false
	for _, ev := range out.Events {
		if ev.ToState != nil && *ev.ToState == "ORD_REQUESTED" {
			found = true
		}
	}
	require.True(t, found, "event chain must contain ORD_REQUESTED")

	// DB 只追加：事件表仅 INSERT 路径（迁移无 UPDATE/DELETE 触发器；代码层无方法）
	var n int
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT COUNT(*) FROM order_events WHERE tenant_id='chang-an' AND order_id=$1`, orderID).Scan(&n))
	require.GreaterOrEqual(t, n, len(out.Events))
}

// TestL1HTTPEvidenceAppendOnly L1-16 经 HTTP · 凭证链只追加：版本递增 + 同版本 PK 冲突拒绝。
func TestL1HTTPEvidenceAppendOnly(t *testing.T) {
	e := testkit.NewEnvWithOptions(t, testkit.Options{Starter: &testkit.NoopStarter{}})
	orderID := e.CreateOrder("款到", "order:chang-an:quick:l1-evidence-1")

	// 直接造备货单 + 发货单（L1 聚焦 DB 契约；FK 顺序：先备货后发货）
	_, err := e.Conn().Exec(t.Context(), `INSERT INTO stock_prepares
		(id, tenant_id, order_id, warehouse_id, state, planned_qty, remaining_qty, batch_seq, idempotency_key)
		VALUES ('STK-l1-ev', 'chang-an', $1, 'wh-yangyuan', 'STK_WAREHOUSE_CONFIRMED', 100, 100, 1, 'sp:l1-ev')`, orderID)
	require.NoError(t, err)
	_, err = e.Conn().Exec(t.Context(), `INSERT INTO shipments
		(id, tenant_id, order_id, stock_prepare_id, warehouse_id, state, planned_qty, idempotency_key)
		VALUES ('SHP-l1-ev', 'chang-an', $1, 'STK-l1-ev', 'wh-yangyuan', 'OPEN', 100, 'shp:l1-ev')`, orderID)
	require.NoError(t, err)

	path := "/api/v1/orders/" + orderID + "/shipments/SHP-l1-ev/evidence"
	rec := e.DoJSON("POST", path, `{"file_url":"s3://e/v1.png","file_hash":"h1"}`, http.StatusCreated,
		map[string]string{"X-User-Id": "u-wh-yangyuan"})
	var out struct {
		Version int `json:"version"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, 1, out.Version)

	// 手工覆盖 v1 → 租户复合 PK 冲突（只追加防线；02 §3.1 租户维度）
	_, err = e.Conn().Exec(t.Context(),
		`INSERT INTO evidence_versions (tenant_id, shipment_id, version, uploader_id, file_url, file_hash)
		 VALUES ('chang-an', 'SHP-l1-ev', 1, 'u-x', 'u', 'h')`)
	require.Error(t, err, "overwrite v1 must violate PK (tenant_id, shipment_id, version)")

	// 其他租户不可见（跨租户隔离）
	var n int
	require.NoError(t, e.Conn().QueryRow(t.Context(),
		`SELECT COUNT(*) FROM evidence_versions WHERE tenant_id='tenant-b' AND shipment_id='SHP-l1-ev'`).Scan(&n))
	require.Zero(t, n)
}

// TestL1HTTPValidation 参数校验：非法请求体/缺必填字段 → 400。
func TestL1HTTPValidation(t *testing.T) {
	e := testkit.NewEnvWithOptions(t, testkit.Options{Starter: &testkit.NoopStarter{}})

	// 非法 JSON
	e.DoJSON("POST", "/api/v1/orders", `{not-json`, http.StatusBadRequest, nil)
	// 缺 idempotency_key / customer_id
	e.DoJSON("POST", "/api/v1/orders", `{"customer_id":"cust-0001"}`, http.StatusBadRequest, nil)
	e.DoJSON("POST", "/api/v1/orders", `{"idempotency_key":"k"}`, http.StatusBadRequest, nil)
	// 未知订单 → 404（订单查找先于信号名校验）；未知信号（订单存在时）→ 400（L2 覆盖）
	e.DoJSON("POST", "/api/v1/orders/ORD-x/signal/nope", `{}`, http.StatusNotFound, nil)
	// 预订单非法体
	e.DoJSON("POST", "/api/v1/preorders", `{bad`, http.StatusBadRequest, nil)
}
