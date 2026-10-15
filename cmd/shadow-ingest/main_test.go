package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"b2b_pilot_v3/internal/sync"
)

// S-04 · 摄入映射：敏感字段剥离 + 事件 ID 派生 + 必填列缺省行为。
func TestSourceEventMappingAndSanitize(t *testing.T) {
	sc := sync.SourceConfig{
		Name: "orders",
		ColumnMap: map[string]string{
			"legacy_flow_id":  "flow_id",
			"legacy_event_id": "event_id",
			"event_action":    "action",
			"operator_id":     "operator",
			"occurred_at":     "occurred_at",
			"stage_code":      "stage_code",
		},
	}
	row := sync.Row{
		"flow_id": "flow-1", "event_id": "e1", "action": "主管通过",
		"operator": "u-supervisor", "occurred_at": "2026-08-20T09:00:00Z",
		"stage_code": "ORD", "open_id": "ou-SECRET",
	}
	clean := sync.SanitizeRow(row)
	ev, err := sourceEvent("chang-an", sc, clean, 0)
	if err != nil {
		t.Fatalf("sourceEvent: %v", err)
	}
	if ev.LegacyFlowID != "flow-1" || ev.LegacyEventID != "e1" || ev.Action != "主管通过" {
		t.Fatalf("event = %+v", ev)
	}
	if _, ok := ev.Payload["open_id"]; ok {
		t.Fatal("sensitive column must be stripped before payload")
	}
	if ev.StageCode != "ORD" || ev.OccurredAt.IsZero() {
		t.Fatalf("stage/time = %+v", ev)
	}
}

func TestSourceEventDerivedIDDeterministic(t *testing.T) {
	sc := sync.SourceConfig{ColumnMap: map[string]string{"legacy_flow_id": "flow_id", "event_action": "action"}}
	row := sync.Row{"flow_id": "flow-9", "action": "创建订单"}
	a, err := sourceEvent("chang-an", sc, row, 0)
	if err != nil {
		t.Fatalf("sourceEvent: %v", err)
	}
	b, err := sourceEvent("chang-an", sc, row, 0)
	if err != nil {
		t.Fatalf("sourceEvent replay: %v", err)
	}
	if a.LegacyEventID == "" || a.LegacyEventID != b.LegacyEventID {
		t.Fatalf("derived event id not deterministic: %q vs %q", a.LegacyEventID, b.LegacyEventID)
	}
}

func TestSourceEventRequiresFlowAndAction(t *testing.T) {
	sc := sync.SourceConfig{ColumnMap: map[string]string{"legacy_flow_id": "flow_id", "event_action": "action"}}
	if _, err := sourceEvent("chang-an", sc, sync.Row{"action": "x"}, 0); err == nil || !strings.Contains(err.Error(), "legacy_flow_id") {
		t.Fatalf("missing flow must fail, got %v", err)
	}
	if _, err := sourceEvent("chang-an", sc, sync.Row{"flow_id": "f"}, 0); err == nil || !strings.Contains(err.Error(), "event_action") {
		t.Fatalf("missing action must fail, got %v", err)
	}
}

// 旧系统真实 JSONL 形态：决策事件无顶层 flow_id，需按 instance_id 关联 orders.jsonl 快照；
// detail 为嵌套对象，FileFetcher 需要把嵌套值紧凑序列化而不是整行解析失败。
func TestRealFeishuJsonlJoinEnrichment(t *testing.T) {
	// FileFetcher 直接读真实形态行（嵌套 detail + 数字字段）
	f, err := os.CreateTemp(t.TempDir(), "events-*.jsonl")
	if err != nil {
		t.Fatalf("tmp file: %v", err)
	}
	_, _ = f.WriteString(`{"event_id":"e1","occurred_at":"2026-08-21T09:00:00Z","instance_id":"i1","task_id":"t1","status":"SUPERVISOR_APPROVED","detail":{"operator_open_id":"ou-1","reason":"ok"},"evidence_path":"/tmp/x.json"}` + "\n")
	f.Close()
	fetcher := sync.FileFetcher{Path: f.Name(), CursorField: "occurred_at"}
	batch, err := fetcher.Fetch(context.Background(), "")
	if err != nil {
		t.Fatalf("fetch nested jsonl: %v", err)
	}
	if len(batch.Rows) != 1 || batch.Rows[0]["status"] != "SUPERVISOR_APPROVED" {
		t.Fatalf("rows = %+v", batch.Rows)
	}
	if !strings.Contains(batch.Rows[0]["detail"], "operator_open_id") {
		t.Fatalf("nested detail must be preserved as compact json: %q", batch.Rows[0]["detail"])
	}

	joinIndex := buildJoinIndex([]sync.Row{
		{"instance_id": "i1", "flow_id": "flow-1", "stage_code": "ORD", "order_id": "SO-1"},
		{"instance_id": "i1", "flow_id": "flow-1b", "stage_code": "ORD", "order_id": "SO-1"}, // 最新快照覆盖
	}, "instance_id")
	sc := sync.SourceConfig{JoinSource: "feishu_orders", JoinKeyField: "instance_id",
		JoinFields: map[string]string{"flow_id": "flow_id", "stage_code": "stage_code", "order_id": "order_id"}}
	row := enrichRow(batch.Rows[0], joinIndex, sc)
	if row["flow_id"] != "flow-1b" || row["stage_code"] != "ORD" || row["order_id"] != "SO-1" {
		t.Fatalf("enrich failed: %+v", row)
	}
	// 已有值不被快照覆盖
	row["flow_id"] = "keep"
	row = enrichRow(row, joinIndex, sc)
	if row["flow_id"] != "keep" {
		t.Fatalf("enrich must not overwrite existing value: %+v", row)
	}
}
