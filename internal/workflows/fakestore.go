package workflows

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"b2b_pilot_v3/internal/domain"
	"b2b_pilot_v3/internal/repo"
)

// ── FakeStore：L2 内存存储（Temporal TestSuite 纯库无服务依赖；docs/03-testing.html §5）──
// 业务语义与 repo 对齐：幂等键唯一、守恒校验（备货≤订单、发货≤备货、库存≥0、开票双维）、
// 事件链留痕；故障钩子（failNextShip/failNextInvoice）支撑 L2-10 拒绝+补偿路径。

type FakeStore struct {
	mu sync.Mutex

	orders               map[string]*repo.Order      // key: tenant/orderID
	orderItems           map[string][]repo.OrderItem // key: tenant/orderID
	grades               map[string]string           // key: tenant/customerID
	inventory            map[string]int64            // key: tenant/warehouse/product
	stockPrepares        map[string]*repo.StockPrepare
	preparedByItem       map[string]int64 // key: tenant/orderID/orderItemID
	shipments            map[string]*repo.Shipment
	evidence             map[string][]repo.EvidenceVersion // key: tenant|shipment
	shippedByItem        map[string]int64                  // key: tenant/orderID/orderItemID
	invoices             map[string]*repo.Invoice
	invoicedByItem       map[string]int64 // key: tenant/orderID/orderItemID/qty
	invoicedAmountByItem map[string]int64
	preOrders            map[string]*repo.PreOrder
	tasks                map[string]*repo.ApprovalTask
	events               []string
	payments             []string         // 回款登记留痕（A4 三金额）
	gkEvents             map[string]int64 // 守门员确认幂等（tenant/order/contentHash → 事件 ID）
	invoiceLinks         map[string]int64 // 发票关联（tenant/invoiceID/orderID → 金额）

	// 故障钩子（L2-10）
	failNextShip    bool
	failNextInvoice bool
}

func newFakeStore() *FakeStore {
	return NewFakeStoreForTest()
}

// NewFakeStoreForTest 构造内存存储（L2/api 测试与 LocalRunner 共用）。
func NewFakeStoreForTest() *FakeStore {
	return &FakeStore{
		orders:               map[string]*repo.Order{},
		orderItems:           map[string][]repo.OrderItem{},
		grades:               map[string]string{},
		inventory:            map[string]int64{},
		stockPrepares:        map[string]*repo.StockPrepare{},
		preparedByItem:       map[string]int64{},
		shipments:            map[string]*repo.Shipment{},
		evidence:             map[string][]repo.EvidenceVersion{},
		shippedByItem:        map[string]int64{},
		invoices:             map[string]*repo.Invoice{},
		invoicedByItem:       map[string]int64{},
		invoicedAmountByItem: map[string]int64{},
		preOrders:            map[string]*repo.PreOrder{},
		tasks:                map[string]*repo.ApprovalTask{},
		gkEvents:             map[string]int64{},
		invoiceLinks:         map[string]int64{},
	}
}

func fk(parts ...string) string {
	s := parts[0]
	for _, p := range parts[1:] {
		s += "/" + p
	}
	return s
}

// SeedInvoice 直造一张发票 + 主关联（0.1 A4 端点测试用）。
func (f *FakeStore) SeedInvoice(tenantID, invoiceID, orderID string, amountCents, qty int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invoices[fk(tenantID, invoiceID)] = &repo.Invoice{
		ID: invoiceID, TenantID: tenantID, OrderID: orderID, State: domain.InvoiceStateIssued,
		AmountCents: amountCents, Qty: qty, InvoicedCents: amountCents,
	}
	f.invoiceLinks[fk(tenantID, invoiceID, orderID)] = amountCents
}

func (f *FakeStore) seedOrder(tenantID, orderID, customerID, paymentTerm string, qty int64) *repo.Order {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fk(tenantID, orderID)
	ord := &repo.Order{
		ID: orderID, TenantID: tenantID, CustomerID: customerID,
		State: domain.StateORDRequested, PaymentTerm: paymentTerm, SupplierChannel: "渠道A",
		IdempotencyKey: "order:" + tenantID + ":quick:" + orderID, Payload: []byte(`{}`), PayloadHash: "h",
	}
	f.orders[key] = ord
	grade := "A"
	if customerID == "cust-0002" {
		grade = "C" // C/D 触发董事长审批
	}
	f.grades[fk(tenantID, customerID)] = grade
	f.orderItems[key] = []repo.OrderItem{{
		ID: "item-1", TenantID: tenantID, OrderID: orderID, ProductID: "prod-c68",
		WarehouseID: strPtr("wh-yangyuan"), Quantity: qty, UnitPriceCents: 1000,
	}}
	return ord
}

func (f *FakeStore) order(tenantID, orderID string) *repo.Order {
	return f.orders[fk(tenantID, orderID)]
}

// setOrderItem 覆写订单明细（测试注入收货地址/仓库/价格等；0.1 A1）。
func (f *FakeStore) setOrderItem(tenantID, orderID, itemID, productID, warehouseID string, qty, unitPriceCents int64, receiveAddress string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orderItems[fk(tenantID, orderID)] = []repo.OrderItem{{
		ID: itemID, TenantID: tenantID, OrderID: orderID, ProductID: productID,
		WarehouseID: strPtr(warehouseID), Quantity: qty, UnitPriceCents: unitPriceCents,
		ReceiveAddress: receiveAddress,
	}}
}

func (f *FakeStore) setInventory(tenantID, warehouseID, productID string, qty int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inventory[fk(tenantID, warehouseID, productID)] = qty
}

// ── Store 接口实现 ──

func (f *FakeStore) GetOrder(ctx context.Context, tenantID, orderID string) (*repo.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o := f.orders[fk(tenantID, orderID)]; o != nil {
		cp := *o
		return &cp, nil
	}
	return nil, &repo.ErrNotFound{What: "order " + orderID}
}

func (f *FakeStore) GetOrderItems(ctx context.Context, tenantID, orderID string) ([]repo.OrderItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := f.orderItems[fk(tenantID, orderID)]
	out := make([]repo.OrderItem, len(items))
	copy(out, items)
	return out, nil
}

func (f *FakeStore) GetCustomerGrade(ctx context.Context, tenantID, customerID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.grades[fk(tenantID, customerID)]
	if !ok {
		return "", &repo.ErrNotFound{What: "customer " + customerID}
	}
	return g, nil
}

func (f *FakeStore) CreateOrder(ctx context.Context, in repo.NewOrder) (*repo.Order, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// 客户/产品存在性不再由 Store 负责（D-006：订单数据实体经 orderdata.Provider/pgAPI 校验）
	for _, o := range f.orders {
		if o.TenantID == in.TenantID && o.IdempotencyKey == in.IdempotencyKey {
			if o.PayloadHash != repo.HashPayload(in.Payload) {
				return nil, false, &repo.ErrIdempotencyConflict{Key: in.IdempotencyKey}
			}
			cp := *o
			return &cp, false, nil
		}
	}
	id := "ORD-" + shortFakeID(in.TenantID+":"+in.IdempotencyKey)
	ord := &repo.Order{
		ID: id, TenantID: in.TenantID, CustomerID: in.CustomerID, State: domain.StateORDRequested,
		PaymentTerm: in.PaymentTerm, SupplierChannel: in.SupplierChannel,
		IdempotencyKey: in.IdempotencyKey, Payload: in.Payload, PayloadHash: repo.HashPayload(in.Payload),
	}
	f.orders[fk(in.TenantID, id)] = ord
	for _, it := range in.Items {
		f.orderItems[fk(in.TenantID, id)] = append(f.orderItems[fk(in.TenantID, id)], repo.OrderItem{
			ID: it.ID, TenantID: in.TenantID, OrderID: id, ProductID: it.ProductID,
			WarehouseID: it.WarehouseID, Quantity: it.Quantity, UnitPriceCents: it.UnitPriceCents,
			ReceiveAddress: it.ReceiveAddress,
		})
	}
	f.events = append(f.events, "OrderRequested:"+id)
	return ord, true, nil
}

func (f *FakeStore) UpdateOrderStateTx(ctx context.Context, tenantID, orderID, fromState, toState, action, operator, reason string, detail []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orders[fk(tenantID, orderID)]
	if o == nil {
		return &repo.ErrNotFound{What: "order " + orderID}
	}
	if fromState != "" && o.State != fromState {
		return fmt.Errorf("state mismatch: want %s have %s", fromState, o.State)
	}
	o.State = toState
	f.events = append(f.events, fmt.Sprintf("%s:%s->%s", action, fromState, toState))
	return nil
}

func (f *FakeStore) UpdateOrderMetaTx(ctx context.Context, tenantID, orderID, contractNo string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orders[fk(tenantID, orderID)]
	if o == nil {
		return &repo.ErrNotFound{What: "order " + orderID}
	}
	if contractNo != "" {
		o.ContractNo = &contractNo
	}
	if payload != nil {
		o.Payload = payload
	}
	return nil
}

func (f *FakeStore) SetOrderContractTx(ctx context.Context, tenantID, orderID, contractNo string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orders[fk(tenantID, orderID)]
	if o == nil {
		return &repo.ErrNotFound{What: "order " + orderID}
	}
	o.ContractNo = &contractNo
	return nil
}

func (f *FakeStore) UpdateResubmitChainTx(ctx context.Context, tenantID, orderID string, revision int, fromOrderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orders[fk(tenantID, orderID)]
	if o == nil {
		return &repo.ErrNotFound{What: "order " + orderID}
	}
	o.ResubmitRevision = revision
	o.ResubmitFromOrderID = &fromOrderID
	o.SubmissionMode = "resubmit"
	return nil
}

func (f *FakeStore) ReserveStockTx(ctx context.Context, sp repo.StockPrepare, items []repo.StockPrepareItem, fromState, toState, operator, reason string, detail []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fk(sp.TenantID, sp.ID)
	if cur, exists := f.stockPrepares[key]; !exists {
		// 备货守恒：∑备货 ≤ 订单量（幂等重放不重复计数）
		for _, it := range items {
			prepared := f.preparedByItem[fk(sp.TenantID, sp.OrderID, it.OrderItemID)]
			if err := domain.AssertPrepareWithinOrder(prepared+it.Qty, f.orderQty(sp.TenantID, sp.OrderID, it.OrderItemID)); err != nil {
				return err
			}
			f.preparedByItem[fk(sp.TenantID, sp.OrderID, it.OrderItemID)] = prepared + it.Qty
		}
		sp.State = toState
		f.stockPrepares[key] = &sp
	} else {
		// D-02：仓管退回后商务重新选仓 → 幂等键不变，更新仓库与状态（不重复计备货量）
		cur.WarehouseID = sp.WarehouseID
		cur.State = toState
	}
	return f.setStateLocked(sp.TenantID, sp.OrderID, fromState, toState, "StockAssigned", operator, reason)
}

func (f *FakeStore) ShipAndUpdateOrderTx(ctx context.Context, ship repo.Shipment, items []repo.ShipmentItem, fromState, toState, operator, reason string, detail []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNextShip {
		f.failNextShip = false
		return fmt.Errorf("inject: inventory insufficient")
	}
	key := fk(ship.TenantID, ship.ID)
	if _, exists := f.shipments[key]; !exists {
		// M1（0.1 暂缓）：锁库存不做——不扣库存；发货守恒照常（∑发货 ≤ ∑备货）
		for _, it := range items {
			shipped := f.shippedByItem[fk(ship.TenantID, ship.OrderID, it.OrderItemID)]
			prepared := f.preparedByItem[fk(ship.TenantID, ship.OrderID, it.OrderItemID)]
			if err := domain.AssertShipWithinPrepare(shipped+it.Qty, prepared); err != nil {
				return err
			}
			f.shippedByItem[fk(ship.TenantID, ship.OrderID, it.OrderItemID)] = shipped + it.Qty
		}
		ship.State = domain.ShipmentStateShipped
		f.shipments[key] = &ship
	}
	return f.setStateLocked(ship.TenantID, ship.OrderID, fromState, toState, "GoodsShipped", operator, reason)
}

func (f *FakeStore) InvoiceAndUpdateOrderTx(ctx context.Context, inv repo.Invoice, items []repo.InvoiceItem, fromState, toState, operator, reason string, detail []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNextInvoice {
		f.failNextInvoice = false
		return fmt.Errorf("inject: invoice conservation failed")
	}
	// 开票守恒：金额+数量双维（≤ 有效发货）
	for _, it := range items {
		shipped := f.shippedByItem[fk(inv.TenantID, inv.OrderID, it.OrderItemID)]
		invoicedQty := f.invoicedByItem[fk(inv.TenantID, inv.OrderID, it.OrderItemID)]
		invoicedAmount := f.invoicedAmountByItem[fk(inv.TenantID, inv.OrderID, it.OrderItemID)]
		unitPrice := f.orderUnitPrice(inv.TenantID, inv.OrderID, it.OrderItemID)
		if err := domain.AssertInvoiceWithinShipped(invoicedQty+it.Qty, invoicedAmount+it.AmountCents,
			shipped, shipped*unitPrice); err != nil {
			return err
		}
		f.invoicedByItem[fk(inv.TenantID, inv.OrderID, it.OrderItemID)] = invoicedQty + it.Qty
		f.invoicedAmountByItem[fk(inv.TenantID, inv.OrderID, it.OrderItemID)] = invoicedAmount + it.AmountCents
	}
	inv.State = domain.InvoiceStateIssued
	f.invoices[fk(inv.TenantID, inv.ID)] = &inv
	return f.setStateLocked(inv.TenantID, inv.OrderID, fromState, toState, "InvoiceIssued", operator, reason)
}

func (f *FakeStore) SumShippedByOrderItem(ctx context.Context, tenantID, orderID string) (map[string]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int64{}
	for k, v := range f.shippedByItem {
		if k == fk(tenantID, orderID) {
			out[k] = v
		}
	}
	// 精确按 item 键提取
	res := map[string]int64{}
	for _, it := range f.orderItems[fk(tenantID, orderID)] {
		res[it.ID] = f.shippedByItem[fk(tenantID, orderID, it.ID)]
	}
	_ = out
	return res, nil
}

func (f *FakeStore) CountShipments(ctx context.Context, tenantID, orderID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, sh := range f.shipments {
		if sh.TenantID == tenantID && sh.OrderID == orderID {
			n++
		}
	}
	return n, nil
}

// AppendEvidence 追加凭证版本（只追加：版本递增；shipment 不存在拒绝；无覆盖路径）。
func (f *FakeStore) AppendEvidence(ctx context.Context, tenantID, shipmentID, uploaderID, fileURL, fileHash string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fk(tenantID, shipmentID)
	if _, ok := f.shipments[fk(tenantID, shipmentID)]; !ok {
		return 0, fmt.Errorf("shipment %s not found in tenant %s", shipmentID, tenantID)
	}
	vs := f.evidence[key]
	ver := len(vs) + 1
	f.evidence[key] = append(vs, repo.EvidenceVersion{
		TenantID: tenantID, ShipmentID: shipmentID, Version: ver,
		UploaderID: uploaderID, FileURL: fileURL, FileHash: fileHash,
	})
	return ver, nil
}

// TrackPaymentTx 回款登记（0.1 A4：发票收款金额快照；fake 为 no-op 记账）。
func (f *FakeStore) TrackPaymentTx(ctx context.Context, tenantID, orderID string, amount int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.payments = append(f.payments, orderID)
	return nil
}

// AppendEvidenceMeta 追加凭证（携带识别元数据；fake 与 AppendEvidence 同语义 + meta 留痕）。
func (f *FakeStore) AppendEvidenceMeta(ctx context.Context, tenantID, shipmentID, uploaderID, fileURL, fileHash string, meta repo.EvidenceMeta) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fk(tenantID, shipmentID)
	if _, ok := f.shipments[key]; !ok {
		return 0, fmt.Errorf("shipment %s not found in tenant %s", shipmentID, tenantID)
	}
	vs := f.evidence[key]
	ver := len(vs) + 1
	f.evidence[key] = append(vs, repo.EvidenceVersion{
		TenantID: tenantID, ShipmentID: shipmentID, Version: ver,
		UploaderID: uploaderID, FileURL: fileURL, FileHash: fileHash,
		Kind: meta.Kind, Source: meta.Source, InvoiceNo: meta.InvoiceNo,
		AmountCents: meta.AmountCents, TaxCents: meta.TaxCents,
	})
	return ver, nil
}

// ShipmentShippedAmountCents 发货单已发货金额（fake：qty × 单价近似；N-06 测试）。
func (f *FakeStore) ShipmentShippedAmountCents(ctx context.Context, tenantID, shipmentID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.shipments[fk(tenantID, shipmentID)]
	if s == nil {
		return 0, &repo.ErrNotFound{What: "shipment " + shipmentID}
	}
	return s.ShippedQty * f.orderUnitPrice(tenantID, s.OrderID, "item-1"), nil
}

// ── 0.1 A4：发票-订单关联与三金额（fake 语义：L2/api 测试与 LocalRunner 共用）──

// invoiceLinks 发票关联（key: tenant/invoiceID/orderID）。
func (f *FakeStore) LinkInvoiceOrder(ctx context.Context, tenantID, invoiceID, orderID string, amountCents, qty int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fk(tenantID, invoiceID, orderID)
	if _, ok := f.invoiceLinks[key]; ok {
		return false, nil
	}
	// 守恒：该订单未开票额度 = 发货金额 - 已关联金额
	if f.orders[fk(tenantID, orderID)] == nil {
		return false, &repo.ErrNotFound{What: "order " + orderID}
	}
	linked := f.linkedCentsLocked(tenantID, orderID)
	shipped := f.shippedCentsLocked(tenantID, orderID)
	if amountCents > shipped-linked {
		return false, &domain.ErrConservation{Kind: "invoice", Msg: "link amount exceeds uninvoiced"}
	}
	f.invoiceLinks[key] = amountCents
	return true, nil
}

// shippedCentsLocked 订单已发货金额（∑ 明细发货量 × 单价；调用方持锁）。
func (f *FakeStore) shippedCentsLocked(tenantID, orderID string) int64 {
	var total int64
	for _, it := range f.orderItems[fk(tenantID, orderID)] {
		total += f.shippedByItem[fk(tenantID, orderID, it.ID)] * it.UnitPriceCents
	}
	return total
}

// linkedCentsLocked 订单已关联开票金额（调用方持锁）。
func (f *FakeStore) linkedCentsLocked(tenantID, orderID string) int64 {
	var linked int64
	suffix := "/" + orderID
	for k, amt := range f.invoiceLinks {
		if len(k) > len(suffix) && k[len(k)-len(suffix):] == suffix {
			linked += amt
		}
	}
	return linked
}

// ListInvoiceLinks 发票全部关联。
func (f *FakeStore) ListInvoiceLinks(ctx context.Context, tenantID, invoiceID string) ([]repo.InvoiceOrderLink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []repo.InvoiceOrderLink
	prefix := tenantID + "/" + invoiceID + "/"
	for k, amt := range f.invoiceLinks {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, repo.InvoiceOrderLink{TenantID: tenantID, InvoiceID: invoiceID,
				OrderID: k[len(prefix):], AmountCents: amt})
		}
	}
	return out, nil
}

// GetInvoice 查询发票（fake）。
func (f *FakeStore) GetInvoice(ctx context.Context, tenantID, invoiceID string) (*repo.Invoice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inv := f.invoices[fk(tenantID, invoiceID)]
	if inv == nil {
		return nil, &repo.ErrNotFound{What: "invoice " + invoiceID}
	}
	cp := *inv
	return &cp, nil
}

// OrderThreeAmounts 订单三金额（fake：发货/开票/收款）。
func (f *FakeStore) OrderThreeAmounts(ctx context.Context, tenantID, orderID string) (*repo.OrderThreeAmounts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.orders[fk(tenantID, orderID)] == nil {
		return nil, &repo.ErrNotFound{What: "order " + orderID}
	}
	shipped := f.shippedCentsLocked(tenantID, orderID)
	invoiced := f.linkedCentsLocked(tenantID, orderID)
	var received int64
	for _, p := range f.payments {
		if p == orderID {
			received += shipped
		}
	}
	return &repo.OrderThreeAmounts{OrderID: orderID, ReceivedCents: received, ShippedCents: shipped,
		InvoicedCents: invoiced, UninvoicedCents: shipped - invoiced}, nil
}

// gkConfirmations 守门员确认事件（key: tenant/order/contentHash → 事件 ID）。
var _ = 0

// AppendGatekeeperConfirmation 守门员确认（fake：内容指纹幂等；N-01 测试）。
func (f *FakeStore) AppendGatekeeperConfirmation(ctx context.Context, tenantID, orderID, contentHash string, detail []byte, operator string) (eventID int64, already bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := tenantID + "/" + orderID + "/" + contentHash
	if id, ok := f.gkEvents[key]; ok {
		return id, true, nil
	}
	id := int64(len(f.gkEvents) + 1)
	f.gkEvents[key] = id
	f.events = append(f.events, "GatekeeperConfirmed:"+orderID)
	return id, false, nil
}

// ListEvidenceVersions 回放全部版本。
func (f *FakeStore) ListEvidenceVersions(ctx context.Context, tenantID, shipmentID string) ([]repo.EvidenceVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]repo.EvidenceVersion{}, f.evidence[fk(tenantID, shipmentID)]...), nil
}

// ShipmentFor 返回内部发货单指针（测试直改状态用）。
func (f *FakeStore) ShipmentFor(tenantID, shipmentID string) *repo.Shipment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shipments[fk(tenantID, shipmentID)]
}

// StockPrepareFor 返回内部备货单指针（测试断言选仓/收货地址）。
func (f *FakeStore) StockPrepareFor(tenantID, spID string) *repo.StockPrepare {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stockPrepares[fk(tenantID, spID)]
}

// InventoryOf 查询库存（N-03 锁库存暂缓断言：备货不扣库存）。
func (f *FakeStore) InventoryOf(tenantID, warehouseID, productID string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inventory[fk(tenantID, warehouseID, productID)]
}

// Shipments 全部发货单（N-03 选仓继承断言）。
func (f *FakeStore) Shipments() []*repo.Shipment {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*repo.Shipment, 0, len(f.shipments))
	for _, s := range f.shipments {
		cp := *s
		out = append(out, &cp)
	}
	return out
}

// GetStockPrepare 读备货单（选仓生效到发货）。
func (f *FakeStore) GetStockPrepare(ctx context.Context, tenantID, stockPrepareID string) (*repo.StockPrepare, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sp := f.stockPrepares[fk(tenantID, stockPrepareID)]
	if sp == nil {
		return nil, &repo.ErrNotFound{What: "stock_prepare " + stockPrepareID}
	}
	cp := *sp
	return &cp, nil
}

func (f *FakeStore) GetShipment(ctx context.Context, tenantID, shipmentID string) (*repo.Shipment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.shipments[fk(tenantID, shipmentID)]; s != nil {
		cp := *s
		return &cp, nil
	}
	return nil, &repo.ErrNotFound{What: "shipment " + shipmentID}
}

func (f *FakeStore) CreateApprovalTaskTx(ctx context.Context, in repo.NewApprovalTask) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tasks {
		if t.TenantID == in.TenantID && t.OrderID == in.OrderID && t.Kind == in.Kind && t.State == "PENDING" {
			return t.ID, nil
		}
	}
	id := "AT-" + fmt.Sprintf("%d", len(f.tasks)+1)
	f.tasks[id] = &repo.ApprovalTask{
		ID: id, TenantID: in.TenantID, OrderID: in.OrderID, StageCode: in.StageCode, Kind: in.Kind,
		State: "PENDING", AssigneeID: in.AssigneeID, CreatedAt: time.Now(),
	}
	return id, nil
}

func (f *FakeStore) DecideTaskTx(ctx context.Context, tenantID, taskID, toState, verdict, decidedBy string, detail []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.tasks[taskID]
	if t == nil {
		return &repo.ErrNotFound{What: "task " + taskID}
	}
	if t.State != "PENDING" {
		return &repo.ErrAlreadyExists{Msg: "task already decided"}
	}
	t.State = toState
	t.Verdict = &verdict
	t.DecidedBy = &decidedBy
	return nil
}

func (f *FakeStore) GetApprovalTask(ctx context.Context, tenantID, taskID string) (*repo.ApprovalTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.tasks[taskID]; t != nil {
		return t, nil
	}
	return nil, &repo.ErrNotFound{What: "task " + taskID}
}

func (f *FakeStore) ListApprovalTasks(ctx context.Context, tenantID string, assignee, kind, state string, limit int) ([]repo.ApprovalTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []repo.ApprovalTask
	for _, t := range f.tasks {
		if t.TenantID != tenantID {
			continue
		}
		if assignee != "" && (t.AssigneeID == nil || *t.AssigneeID != assignee) {
			continue
		}
		if kind != "" && t.Kind != kind {
			continue
		}
		if state != "" && t.State != state {
			continue
		}
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *FakeStore) CreatePreOrder(ctx context.Context, in repo.NewPreOrder) (*repo.PreOrder, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.preOrders {
		if p.TenantID == in.TenantID && p.GroupID == in.GroupID && p.MsgID == in.MsgID {
			return p, false, nil
		}
	}
	p := &repo.PreOrder{
		ID: "PRE-" + fmt.Sprintf("%d", len(f.preOrders)+1), TenantID: in.TenantID, GroupID: in.GroupID,
		MsgID: in.MsgID, State: domain.StatePreCaptured, Candidates: in.Candidates, IdempotencyKey: in.IdempotencyKey,
	}
	f.preOrders[p.ID] = p
	f.events = append(f.events, "PreOrderCaptured:"+p.ID)
	return p, true, nil
}

func (f *FakeStore) GetPreOrder(ctx context.Context, tenantID, preOrderID string) (*repo.PreOrder, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.preOrders[preOrderID]; p != nil {
		cp := *p
		return &cp, nil
	}
	return nil, &repo.ErrNotFound{What: "preorder " + preOrderID}
}

func (f *FakeStore) UpdatePreOrderStateTx(ctx context.Context, tenantID, preOrderID, fromState, toState, action, reason string, detail []byte, operator string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.preOrders[preOrderID]
	if p == nil {
		return &repo.ErrNotFound{What: "preorder " + preOrderID}
	}
	p.State = toState
	f.events = append(f.events, fmt.Sprintf("%s:%s->%s", action, fromState, toState))
	return nil
}

func (f *FakeStore) ListOrderEvents(ctx context.Context, tenantID, orderID string, limit int) ([]repo.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []repo.Event
	for _, e := range f.events {
		out = append(out, repo.Event{AggregateID: orderID, Action: e})
	}
	return out, nil
}

func (f *FakeStore) ListPreOrderEvents(ctx context.Context, tenantID, preOrderID string) ([]repo.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []repo.Event
	for _, e := range f.events {
		out = append(out, repo.Event{AggregateID: preOrderID, Action: e})
	}
	return out, nil
}

func (f *FakeStore) RecordFlowFailed(ctx context.Context, tenantID, orderID, reason string, detail []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "FlowFailed:"+reason)
	return nil
}

// ── 内部辅助 ──

func (f *FakeStore) orderQty(tenantID, orderID, itemID string) int64 {
	for _, it := range f.orderItems[fk(tenantID, orderID)] {
		if it.ID == itemID {
			return it.Quantity
		}
	}
	return 0
}

func (f *FakeStore) orderUnitPrice(tenantID, orderID, itemID string) int64 {
	for _, it := range f.orderItems[fk(tenantID, orderID)] {
		if it.ID == itemID {
			return it.UnitPriceCents
		}
	}
	return 0
}

func (f *FakeStore) setStateLocked(tenantID, orderID, fromState, toState, action, operator, reason string) error {
	o := f.orders[fk(tenantID, orderID)]
	if o == nil {
		return &repo.ErrNotFound{What: "order " + orderID}
	}
	if fromState != "" && o.State != fromState {
		return fmt.Errorf("state mismatch: want %s have %s", fromState, o.State)
	}
	o.State = toState
	f.events = append(f.events, fmt.Sprintf("%s:%s->%s", action, fromState, toState))
	return nil
}

func shortFakeID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:6])
}

// ── API 测试种子辅助 ──

// SeedTenant 种子：租户。
func (f *FakeStore) SeedTenant(tenantID string) {
	_ = tenantID
}

// SeedCustomer 种子：客户（grade A/B/C/D）。
func (f *FakeStore) SeedCustomer(tenantID, customerID, grade string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grades[fk(tenantID, customerID)] = grade
}

// SeedInventory 种子：库存。
func (f *FakeStore) SeedInventory(tenantID, warehouseID, productID string, qty int64) {
	f.setInventory(tenantID, warehouseID, productID, qty)
}

// ── FakeNotifier ──

type FakeNotifier struct {
	mu   sync.Mutex
	sent []string
}

func (n *FakeNotifier) NotifyApproval(ctx context.Context, tenantID, orderID, kind, assignee string, taskID string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, "approval:"+kind+":"+assignee)
	return nil
}

func (n *FakeNotifier) NotifyPreOrderConfirm(ctx context.Context, tenantID, preOrderID string, candidates []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, "preorder-card:"+preOrderID)
	return nil
}

func (n *FakeNotifier) NotifyOrderEvent(ctx context.Context, tenantID, orderID, state string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, "order-event:"+state)
	return nil
}

func (n *FakeNotifier) count(prefix string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, s := range n.sent {
		if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
			c++
		}
	}
	return c
}

// ── 测试构造辅助（api/localrunner 测试用）──

// NewOrderForTest 构造测试订单。
func NewOrderForTest(tenantID, customerID, paymentTerm, idemKey string) repo.NewOrder {
	return repo.NewOrder{
		TenantID: tenantID, CustomerID: customerID, PaymentTerm: paymentTerm, SupplierChannel: "渠道A",
		IdempotencyKey: idemKey, Payload: []byte(`{}`),
		Items: []repo.NewOrderItem{{
			ID: "item-1", ProductID: "prod-c68", WarehouseID: strPtr("wh-yangyuan"),
			Quantity: 100, UnitPriceCents: 1000,
		}},
	}
}

// NewPreOrderForTest 构造测试预订单。
func NewPreOrderForTest(tenantID, groupID, msgID string) repo.NewPreOrder {
	return repo.NewPreOrder{
		TenantID: tenantID, GroupID: groupID, MsgID: msgID,
		Candidates:     []byte(`[{"product_sku":"SKU-C68","product_id":"prod-c68","quantity":10,"unit_price_cents":1000,"confidence":0.95}]`),
		IdempotencyKey: "pre:" + tenantID + ":" + groupID + ":" + msgID + ":v1",
	}
}

// Orders 返回全部订单（测试断言）。
func (f *FakeStore) Orders() []*repo.Order {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*repo.Order
	for _, o := range f.orders {
		out = append(out, o)
	}
	return out
}

// SeedPrepared 种子备货量（段恢复测试用：直接定位到中间状态；
// 同时写入备货聚合记录，使工作流重入时的幂等备货跳过重复计数）。
// SeedShipment 测试辅助：直接造一个发货单（凭证链测试用）。
func (f *FakeStore) SeedShipment(tenantID, orderID, shipmentID, warehouseID string, plannedQty int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shipments[fk(tenantID, shipmentID)] = &repo.Shipment{
		ID: shipmentID, TenantID: tenantID, OrderID: orderID,
		StockPrepareID: "STK-x", WarehouseID: warehouseID, State: domain.ShipmentStateOpen,
		PlannedQty: plannedQty, IdempotencyKey: "shp:" + shipmentID,
	}
}

func (f *FakeStore) SeedPrepared(tenantID, orderID, itemID string, qty int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.preparedByItem[fk(tenantID, orderID, itemID)] = qty
	spID := "STK-" + shortFakeID(orderID+":1")
	if _, ok := f.stockPrepares[fk(tenantID, spID)]; !ok {
		f.stockPrepares[fk(tenantID, spID)] = &repo.StockPrepare{
			ID: spID, TenantID: tenantID, OrderID: orderID, WarehouseID: "wh-yangyuan",
			State: "STK_WAREHOUSE_CONFIRM", PlannedQty: qty, RemainingQty: qty, BatchSeq: 1,
			IdempotencyKey: "seed-prepared",
		}
	}
}

// SeedShipped 种子已发货量（段恢复测试用）。
func (f *FakeStore) SeedShipped(tenantID, orderID, itemID string, qty int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shippedByItem[fk(tenantID, orderID, itemID)] = qty
	f.inventory[fk(tenantID, "wh-yangyuan", "prod-c68")] -= qty
}
