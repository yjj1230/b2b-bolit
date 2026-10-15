package pgapi

import (
	"encoding/json"
	"testing"
)

func TestRawToTextConvergesLooseShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"null", `null`, ""},
		{"string", `"月结30天"`, "月结30天"},
		{"number", `123`, "123"},
		{"text array", `[{"text":"科技","type":"x"},{"text":"制造","type":"y"}]`, "科技,制造"},
		{"object fallback", `{"text":"abc"}`, `{"text":"abc"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rawToText(json.RawMessage(tc.raw)); got != tc.want {
				t.Fatalf("rawToText(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestRawToTextListConvergesLooseShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"null", `null`, nil},
		{"plain string", `"已发货"`, []string{"已发货"}},
		{"json array string", `"[{\"text\":\"已发货\"},{\"text\":\"在途\"}]"`, []string{"已发货", "在途"}},
		{"array", `[{"text":"25kg"},{"text":"50kg"}]`, []string{"25kg", "50kg"}},
		{"object fallback", `{"text":"abc"}`, []string{`{"text":"abc"}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rawToTextList(json.RawMessage(tc.raw))
			if len(got) != len(tc.want) {
				t.Fatalf("rawToTextList(%s) = %#v, want %#v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("rawToTextList(%s) = %#v, want %#v", tc.raw, got, tc.want)
				}
			}
		})
	}
}

func TestRawToFloatListConvergesLooseShapes(t *testing.T) {
	if got := rawToFloatList(json.RawMessage(`12.5`)); len(got) != 1 || got[0] != 12.5 {
		t.Fatalf("scalar: %#v", got)
	}
	if got := rawToFloatList(json.RawMessage(`[1.5,2.5]`)); len(got) != 2 || got[0] != 1.5 {
		t.Fatalf("array: %#v", got)
	}
	if got := rawToFloatList(json.RawMessage(`"12.5"`)); got != nil {
		t.Fatalf("invalid string should be nil: %#v", got)
	}
	if got := rawToFloatList(json.RawMessage(`null`)); got != nil {
		t.Fatalf("null should be nil: %#v", got)
	}
}

func TestCustomerUnmarshalLooseShapes(t *testing.T) {
	var c Customer
	err := json.Unmarshal([]byte(`{
		"record_id":"rec-c",
		"customer_no": 10086,
		"customer_name":"测试客户",
		"sales_users":[{"id":"u1","name":"虹励"}],
		"department":[{"text":"一部","type":"dept"}],
		"payment_terms":[{"text":"月结30天"}],
		"customer_level":"A"
	}`), &c)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.RecordID != "rec-c" || c.CustomerName != "测试客户" || c.CustomerNo != "10086" ||
		c.CustomerLevel != "A" || c.Department != "一部" || c.PaymentTerms != "月结30天" {
		t.Fatalf("unexpected customer: %+v", c)
	}
	if len(c.SalesUsers) != 1 || c.SalesUsers[0].Name != "虹励" {
		t.Fatalf("sales_users not preserved: %+v", c.SalesUsers)
	}
}

func TestCustomerUnmarshalMissingFields(t *testing.T) {
	var c Customer
	if err := json.Unmarshal([]byte(`{"record_id":"rec-c"}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.RecordID != "rec-c" || c.CustomerName != "" || c.SalesUsers != nil {
		t.Fatalf("unexpected customer: %+v", c)
	}
}

func TestPurchaseContractUnmarshalLooseShapes(t *testing.T) {
	var pc PurchaseContract
	err := json.Unmarshal([]byte(`{
		"record_id":"rec-ct",
		"contract_no":"HT-1",
		"customer_name":"测试客户",
		"spec":[{"text":"25kg","type":"pack"}],
		"quantity_kg": 12.5,
		"ship_status":"[{\"text\":\"已发货\"},{\"text\":\"已签收\"}]"
	}`), &pc)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pc.RecordID != "rec-ct" || pc.ContractNo != "HT-1" || pc.CustomerName != "测试客户" {
		t.Fatalf("plain fields lost: %+v", pc)
	}
	if len(pc.Spec) != 1 || pc.Spec[0] != "25kg" {
		t.Fatalf("spec: %#v", pc.Spec)
	}
	if len(pc.QuantityKg) != 1 || pc.QuantityKg[0] != 12.5 {
		t.Fatalf("quantity: %#v", pc.QuantityKg)
	}
	if len(pc.ShipStatus) != 2 || pc.ShipStatus[0] != "已发货" || pc.ShipStatus[1] != "已签收" {
		t.Fatalf("ship_status: %#v", pc.ShipStatus)
	}
}

func TestListResultJSONShapeMatchesFrontendContract(t *testing.T) {
	in := ListResult[string]{Items: []string{"a"}, Page: 1, PageSize: 20, Total: 1, HasNext: false}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"items":["a"],"page":1,"page_size":20,"total":1,"has_next":false}`
	if string(b) != want {
		t.Fatalf("marshal = %s, want %s", b, want)
	}
	var out ListResult[string]
	if err := json.Unmarshal([]byte(`{"items":["b"],"page":2,"page_size":10,"total":3,"has_next":true}`), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0] != "b" || out.Page != 2 || !out.HasNext {
		t.Fatalf("unmarshal result: %+v", out)
	}
}
