package sync

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// ── sync 包单元测试（L0/L2 纯库；11-migration V1/V4 与映射语义）──

func testSource() SourceConfig {
	return SourceConfig{
		TenantID:    "chang-an",
		Name:        "feishu_orders",
		Kind:        "file",
		Path:        "unused",
		CursorField: "updated_at",
		ColumnMap: map[string]string{
			"order_id": "order_id", "customer_id": "customer_id", "state": "state",
			"payment_term": "payment_term", "supplier_channel": "supplier_channel",
			"idempotency_key": "idempotency_key", "legacy_flow_id": "flow_id",
			"event_action": "event_action", "event_reason": "event_reason",
			"operator_id": "operator_open_id", "occurred_at": "occurred_at",
			"legacy_stage_id": "stage_id", "legacy_instance_id": "instance_id",
			"legacy_task_id": "task_id", "stage_code": "stage_code",
		},
	}
}

func TestMapRowFullMapping(t *testing.T) {
	sc := testSource()
	row := Row{
		"order_id": "ORD-SYNC-1", "customer_id": "cust-0001", "state": "已开单",
		"payment_term": "非款到", "supplier_channel": "渠道A", "idempotency_key": "legacy-key-1",
		"flow_id": "FLOW-001", "event_action": "OrderRequested", "event_reason": "飞书同步",
		"operator_open_id": "ou_123", "occurred_at": "2026-08-01T10:00:00+08:00",
		"stage_id": "STG-1", "instance_id": "INST-1", "task_id": "TASK-1", "stage_code": "ORD",
	}
	m, err := MapRow(sc, row)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if m.LegacyFlowID != "FLOW-001" || m.OrderID != "ORD-SYNC-1" {
		t.Fatalf("unexpected mapping: %+v", m)
	}
	if m.LegacyInstanceID != "INST-1" || m.LegacyTaskID != "TASK-1" || m.StageCode != "ORD" {
		t.Fatalf("trace mapping wrong: %+v", m)
	}
	if m.EventAction != "OrderRequested" || m.EventOperator != "ou_123" {
		t.Fatalf("event mapping wrong: %+v", m)
	}
	// 缺 flow_id → 拒绝
	bad := Row{"order_id": "X"}
	if _, err := MapRow(sc, bad); err == nil {
		t.Fatal("missing legacy_flow_id should fail")
	}
	// 缺 order_id/idempotency → 派生确定性键
	derived, err := MapRow(sc, Row{"flow_id": "FLOW-002"})
	if err != nil {
		t.Fatalf("derived map: %v", err)
	}
	if derived.OrderID != "ORD-"+ShortID("legacy:FLOW-002") || derived.IdempotencyKey != "legacy:FLOW-002" {
		t.Fatalf("derived keys wrong: %+v", derived)
	}
}

func TestSanitizeRowSensitiveZeroResidue(t *testing.T) {
	row := Row{
		"order_id": "ORD-1", "open_id": "ou_evil", "operator_open_id": "ou_op",
		"access_token": "tok", "view_token": "vt", "private_key": "k", "encoding_aes_key": "aes",
		"normal_field": "keep",
	}
	out := SanitizeRow(row)
	for _, k := range []string{"open_id", "operator_open_id", "access_token", "view_token", "private_key", "encoding_aes_key"} {
		if _, ok := out[k]; ok {
			t.Fatalf("sensitive key %s should be stripped", k)
		}
	}
	if out["order_id"] != "ORD-1" || out["normal_field"] != "keep" {
		t.Fatalf("normal fields lost: %v", out)
	}
}

func TestBatchChecksumDeterministic(t *testing.T) {
	a := []Row{{"a": "1", "b": "2"}, {"a": "3"}}
	b := []Row{{"a": "3"}, {"a": "1", "b": "2"}}
	c := []Row{{"a": "1", "b": "2"}, {"a": "4"}}
	if BatchChecksum(a) != BatchChecksum(b) {
		t.Fatal("checksum must be order-independent")
	}
	if BatchChecksum(a) == BatchChecksum(c) {
		t.Fatal("different rows must differ")
	}
	if len(BatchChecksum(a)) != 64 {
		t.Fatalf("unexpected checksum length")
	}
}

func TestFileFetcherIncrementalCursor(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "orders.jsonl")
	content := `{"order_id":"ORD-1","flow_id":"F1","updated_at":"2026-08-01T00:00:00Z"}
{"order_id":"ORD-2","flow_id":"F2","updated_at":"2026-08-02T00:00:00Z"}
{"order_id":"ORD-3","flow_id":"F3","updated_at":"2026-08-03T00:00:00Z"}`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &FileFetcher{Path: p, CursorField: "updated_at"}
	all, err := f.Fetch(t.Context(), "")
	if err != nil || len(all.Rows) != 3 {
		t.Fatalf("full fetch: rows=%d err=%v", len(all.Rows), err)
	}
	if all.Cursor != "2026-08-03T00:00:00Z" {
		t.Fatalf("cursor: %s", all.Cursor)
	}
	inc, err := f.Fetch(t.Context(), "2026-08-02T00:00:00Z")
	if err != nil || len(inc.Rows) != 1 || inc.Rows[0]["order_id"] != "ORD-3" {
		t.Fatalf("incremental fetch: rows=%d err=%v", len(inc.Rows), err)
	}
	// 文件不存在 → 空批次（不阻塞）
	missing, err := (&FileFetcher{Path: filepath.Join(dir, "nope.jsonl")}).Fetch(t.Context(), "")
	if err != nil || len(missing.Rows) != 0 {
		t.Fatalf("missing file: %v", err)
	}
}

func TestHTTPFetcherPagedDescWithCursor(t *testing.T) {
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-API-Key") != "k" {
			t.Errorf("missing api key")
		}
		if got := r.URL.Query().Get("sort_by"); got != "last_modified_time" {
			t.Errorf("sort_by = %s", got)
		}
		page := r.URL.Query().Get("page")
		items := []map[string]any{
			{"record_id": "r1", "last_modified_time": "2026-08-18T10:00:00Z", "order_no": "SO-1"},
			{"record_id": "r2", "last_modified_time": "2026-08-18T09:00:00Z", "order_no": "SO-2"},
		}
		hasNext := false
		if page == "1" {
			items = items[:1]
			hasNext = true
		} else if page == "2" {
			items = items[1:]
		} else {
			t.Errorf("unexpected page %s", page)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"items":      items,
				"pagination": map[string]any{"has_next": hasNext},
			},
			"message": "ok",
		})
	}))
	defer ts.Close()

	f := &HTTPFetcher{URL: ts.URL, APIKey: "k", CursorField: "last_modified_time"}
	// since 截断：只应返回 10:00 这一条
	got, err := f.Fetch(t.Context(), "2026-08-18T09:30:00Z")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got.Rows) != 1 || got.Rows[0]["order_no"] != "SO-1" || got.Cursor != "2026-08-18T10:00:00Z" {
		t.Fatalf("since cut: %+v", got)
	}
	// 全量两页
	got, err = f.Fetch(t.Context(), "")
	if err != nil || len(got.Rows) != 2 || got.Cursor != "2026-08-18T10:00:00Z" {
		t.Fatalf("full fetch: %+v err=%v", got, err)
	}
	if calls != 4 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestHTTPFetcherErrors(t *testing.T) {
	if _, err := (&HTTPFetcher{URL: ""}).Fetch(t.Context(), ""); err == nil {
		t.Fatal("empty url must fail")
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Invalid or missing API key"}`))
	}))
	defer ts.Close()
	if _, err := (&HTTPFetcher{URL: ts.URL}).Fetch(t.Context(), ""); err == nil {
		t.Fatal("401 must fail")
	}
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":false,"error_code":"E1","message":"m"}`))
	}))
	defer ts2.Close()
	if _, err := (&HTTPFetcher{URL: ts2.URL}).Fetch(t.Context(), ""); err == nil {
		t.Fatal("business failure must fail")
	}
}

func TestLoadConfigAndFetcherKind(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.json")
	cfg := `{"tenant_id":"chang-an","sources":[{"name":"a","kind":"file","path":"x.jsonl","cursor_field":"u","column_map":{}}]}`
	if err := os.WriteFile(p, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.TenantID != "chang-an" || len(c.Sources) != 1 {
		t.Fatalf("config: %+v", c)
	}
	if _, err := NewFetcher(c.Sources[0]); err != nil {
		t.Fatalf("fetcher: %v", err)
	}
	if _, err := NewFetcher(SourceConfig{Name: "x", Kind: "bogus"}); err == nil {
		t.Fatal("unknown kind should fail")
	}
	// 配置错误分支：文件缺失 / 非法 JSON / 缺租户
	if _, err := LoadConfig(filepath.Join(dir, "nope.json")); err == nil {
		t.Fatal("missing file should fail")
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{not-json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(bad); err == nil {
		t.Fatal("bad json should fail")
	}
	noTenant := filepath.Join(dir, "notenant.json")
	if err := os.WriteFile(noTenant, []byte(`{"sources":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(noTenant); err == nil {
		t.Fatal("missing tenant should fail")
	}
}

// ParseEventTime：RFC3339/空串/非法输入。
func TestParseEventTime(t *testing.T) {
	if _, err := ParseEventTime(""); err != nil {
		t.Fatalf("empty should be ok: %v", err)
	}
	if _, err := ParseEventTime("2026-08-01T10:00:00+08:00"); err != nil {
		t.Fatalf("rfc3339: %v", err)
	}
	if _, err := ParseEventTime("not-a-time"); err == nil {
		t.Fatal("invalid time should fail")
	}
}

// FileFetcher：非法 JSON 行报错；空文件空批次；游标字典序过滤。
func TestFileFetcherMalformedLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(p, []byte("{not-json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (&FileFetcher{Path: p}).Fetch(t.Context(), ""); err == nil {
		t.Fatal("malformed line should fail")
	}
	empty := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(empty, []byte("\n  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := (&FileFetcher{Path: empty}).Fetch(t.Context(), "")
	if err != nil || len(b.Rows) != 0 {
		t.Fatalf("empty file: rows=%d err=%v", len(b.Rows), err)
	}
}

// MapRow：缺列时零值派生；敏感列先剥离（V4 链路）。
func TestMapRowMinimalColumns(t *testing.T) {
	sc := testSource()
	m, err := MapRow(sc, Row{"flow_id": "F1", "open_id": "ou_secret"})
	if err != nil {
		t.Fatalf("map minimal: %v", err)
	}
	if m.PaymentTerm != "款到" || m.StageCode != "ORD" {
		t.Fatalf("defaults wrong: %+v", m)
	}
	if m.Payload == nil || m.PayloadHash == "" {
		t.Fatal("payload missing")
	}
	// SanitizeRow → MapRow 全链路：敏感字段不入 payload
	clean := SanitizeRow(Row{"flow_id": "F1", "operator_open_id": "ou_op", "keep": "v"})
	m2, err := MapRow(sc, clean)
	if err != nil {
		t.Fatalf("map cleaned: %v", err)
	}
	if m2.EventOperator != "" {
		t.Fatalf("sensitive operator must be stripped before mapping: %+v", m2)
	}
}

// BatchChecksum：空批次确定。
func TestBatchChecksumEmpty(t *testing.T) {
	if len(BatchChecksum(nil)) != 64 {
		t.Fatal("empty batch checksum must be valid sha256 hex")
	}
	if BatchChecksum(nil) != BatchChecksum([]Row{}) {
		t.Fatal("nil and empty batch must match")
	}
}

// stringifyValue 全分支（HTTP 源 row 值归一化；COV-01 补强）。
func TestStringifyValueBranches(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"abc", "abc"},
		{true, "true"},
		{false, "false"},
		{3.14, "3.14"},
		{json.Number("7"), "7"},
		{map[string]any{"a": 1}, `{"a":1}`},
		{[]any{1, 2}, `[1,2]`},
	}
	for _, c := range cases {
		if got := stringifyValue(c.in); got != c.want {
			t.Fatalf("stringify(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// SYNC-03 收敛：值级敏感剥离（列名正常但值含 view_token/open_id 也脱敏）。
func TestSanitizeRowValueLevelRedaction(t *testing.T) {
	row := Row{
		"order_id":       "ORD-1",
		"payload_note":   `{"view_token":"abc123","ok":true}`,
		"nested_open_id": "open_id=ou_123",
	}
	out := SanitizeRow(row)
	if out["order_id"] != "ORD-1" {
		t.Fatalf("safe value lost: %+v", out)
	}
	if out["payload_note"] != "[REDACTED]" {
		t.Fatalf("sensitive value not redacted: %+v", out)
	}
	if _, exists := out["nested_open_id"]; exists {
		t.Fatalf("sensitive column must be dropped: %+v", out)
	}
}
