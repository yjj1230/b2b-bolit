//go:build integration

package repo

import (
	"context"
	"testing"
)

// TestBridgeSubmissionRecorder 验证 v0.1 桥接台账回写与幂等。
func TestBridgeSubmissionRecorder(t *testing.T) {
	e := newTestEnv(t)
	defer e.close()
	e.seedWorld()
	ctx := context.Background()

	err := e.repo.RecordSubmission(ctx, "chang-an", "BRG-T1", "SO-BRIDGE-0001", "inst-1", "SO-BRIDGE-0001-ORD-1", "/flows/SO-BRIDGE-0001")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	// 幂等重放：更新而不是报错
	err = e.repo.RecordSubmission(ctx, "chang-an", "BRG-T1", "SO-BRIDGE-0001", "inst-1", "SO-BRIDGE-0001-ORD-1", "/flows/SO-BRIDGE-0001")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	items, err := e.repo.ListBridgeSubmissions(ctx, "chang-an", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].FlowID != "SO-BRIDGE-0001" || items[0].InstanceID != "inst-1" {
		t.Fatalf("items = %+v", items)
	}
}
