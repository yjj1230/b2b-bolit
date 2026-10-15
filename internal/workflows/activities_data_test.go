package workflows

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/orderdata"
	"b2b_pilot_v3/internal/pgapi"
	"b2b_pilot_v3/internal/repo"
)

// dataProv ValidateOrderData 的 pgAPI 替身（覆盖生产数据校验分支）。
type dataProv struct {
	*orderdata.MemoryProvider
	customer    *pgapi.Customer
	products    map[string]*pgapi.Product
	customerErr error
	productErr  error
}

func (p *dataProv) GetCustomer(ctx context.Context, tenantID, customerID string) (*pgapi.Customer, error) {
	if p.customerErr != nil {
		return nil, p.customerErr
	}
	return p.customer, nil
}

func (p *dataProv) GetProduct(ctx context.Context, tenantID, productID string) (*pgapi.Product, error) {
	if p.productErr != nil {
		return nil, p.productErr
	}
	pr, ok := p.products[productID]
	if !ok {
		return nil, errors.New("not found")
	}
	return pr, nil
}

// AUTH/A-03 收敛：ValidateOrderData 生产 pgAPI 分支全覆盖（客户/产品失败与成功）。
func TestValidateOrderDataPGAPIBranches(t *testing.T) {
	items := []repo.OrderItem{
		{ID: "i1", ProductID: "p1", Quantity: 10, UnitPriceCents: 100},
		{ID: "i2", ProductID: "p1", Quantity: 20, UnitPriceCents: 100}, // 重复产品跳过
	}
	a := &Activities{Store: newFakeStore()}

	a.Data = &dataProv{customerErr: errors.New("customer down")}
	_, err := a.ValidateOrderData(t.Context(), "chang-an", "c1", items)
	require.Error(t, err)

	a.Data = &dataProv{customer: &pgapi.Customer{RecordID: "c1", CustomerName: "客户A", CustomerLevel: "A"},
		products:   map[string]*pgapi.Product{"p1": {RecordID: "p1", ProductName: "产品A"}},
		productErr: errors.New("product down")}
	_, err = a.ValidateOrderData(t.Context(), "chang-an", "c1", items)
	require.Error(t, err)

	// 空 product_id 早退
	a.Data = &dataProv{customer: &pgapi.Customer{RecordID: "c1", CustomerLevel: "A"}, products: map[string]*pgapi.Product{}}
	_, err = a.ValidateOrderData(t.Context(), "chang-an", "c1", []repo.OrderItem{{ID: "i1"}})
	require.ErrorContains(t, err, "empty product_id")

	// 成功路径（重复产品只查一次）
	a.Data = &dataProv{customer: &pgapi.Customer{RecordID: "c1", CustomerName: "客户A", CustomerLevel: "B"},
		products: map[string]*pgapi.Product{"p1": {RecordID: "p1", ProductName: "产品A"}}}
	res, err := a.ValidateOrderData(t.Context(), "chang-an", "c1", items)
	require.NoError(t, err)
	require.Equal(t, "B", res.CustomerGrade)
	require.Equal(t, "客户A", res.CustomerName)
}
