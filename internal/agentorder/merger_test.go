package agentorder

import "testing"

func TestMergeAppliesNewFieldsAndReportsMissing(t *testing.T) {
	result := Merge(
		DraftSnapshot{Fields: map[string]FieldValue{
			"product_sku": {Value: "SKU-C68", Source: SourceUser},
		}},
		FieldPatch{Fields: map[string]PatchValue{
			"quantity":        {Value: int64(100), Source: SourceUser, Evidence: "100 件"},
			"receive_address": {Value: "上海", Source: SourceUser, Evidence: "送到上海"},
		}},
		MergePolicy{RequiredFields: []string{"product_sku", "quantity", "receive_address"}},
	)

	if result.Status != MergeStatusCurrent {
		t.Fatalf("status = %s, want CURRENT", result.Status)
	}
	if got := result.Snapshot.Fields["quantity"].Value; got != int64(100) {
		t.Fatalf("quantity = %#v, want 100", got)
	}
	if got := result.Snapshot.Fields["receive_address"].Value; got != "上海" {
		t.Fatalf("receive_address = %#v, want 上海", got)
	}
	if len(result.Missing) != 0 {
		t.Fatalf("missing = %#v, want none", result.Missing)
	}
	if result.Diff["quantity"].Kind != DiffAdded {
		t.Fatalf("quantity diff = %#v, want added", result.Diff["quantity"])
	}
}

func TestMergeKeepsPreviousValueForImplicitConflict(t *testing.T) {
	result := Merge(
		DraftSnapshot{Fields: map[string]FieldValue{
			"product_sku": {Value: "SKU-C68", Source: SourceUser, Evidence: "C68"},
		}},
		FieldPatch{Fields: map[string]PatchValue{
			"product_sku": {Value: "SKU-D90", Source: SourceUser, Evidence: "D90"},
		}},
		MergePolicy{},
	)

	if result.Status != MergeStatusConflict {
		t.Fatalf("status = %s, want CONFLICT", result.Status)
	}
	if got := result.Snapshot.Fields["product_sku"].Value; got != "SKU-C68" {
		t.Fatalf("product = %#v, want previous value", got)
	}
	if len(result.Conflicts) != 1 || result.Conflicts[0].Field != "product_sku" {
		t.Fatalf("conflicts = %#v, want product_sku conflict", result.Conflicts)
	}
	if result.Diff["product_sku"].Kind != DiffConflict {
		t.Fatalf("diff = %#v, want conflict", result.Diff["product_sku"])
	}
}

func TestMergeExplicitReplacementOverridesPreviousValue(t *testing.T) {
	result := Merge(
		DraftSnapshot{Fields: map[string]FieldValue{
			"product_sku": {Value: "SKU-C68", Source: SourceUser},
		}},
		FieldPatch{Fields: map[string]PatchValue{
			"product_sku": {
				Value:               "SKU-D90",
				Source:              SourceUser,
				ExplicitReplacement: true,
				Evidence:            "改成 D90",
			},
		}},
		MergePolicy{},
	)

	if result.Status != MergeStatusCurrent {
		t.Fatalf("status = %s, want CURRENT", result.Status)
	}
	if got := result.Snapshot.Fields["product_sku"].Value; got != "SKU-D90" {
		t.Fatalf("product = %#v, want D90", got)
	}
	if result.Diff["product_sku"].Kind != DiffChanged {
		t.Fatalf("diff = %#v, want changed", result.Diff["product_sku"])
	}
}

func TestMergeUserValueWinsOverModelInference(t *testing.T) {
	result := Merge(
		DraftSnapshot{Fields: map[string]FieldValue{
			"quantity": {Value: int64(50), Source: SourceModel},
		}},
		FieldPatch{Fields: map[string]PatchValue{
			"quantity": {Value: int64(100), Source: SourceUser},
		}},
		MergePolicy{},
	)

	if result.Status != MergeStatusCurrent {
		t.Fatalf("status = %s, want CURRENT", result.Status)
	}
	if got := result.Snapshot.Fields["quantity"].Value; got != int64(100) {
		t.Fatalf("quantity = %#v, want 100", got)
	}
}

func TestMergeHumanValueWinsOverUserValue(t *testing.T) {
	result := Merge(
		DraftSnapshot{Fields: map[string]FieldValue{
			"quantity": {Value: int64(100), Source: SourceUser},
		}},
		FieldPatch{Fields: map[string]PatchValue{
			"quantity": {Value: int64(200), Source: SourceHuman},
		}},
		MergePolicy{},
	)

	if result.Status != MergeStatusCurrent {
		t.Fatalf("status = %s, want CURRENT", result.Status)
	}
	if got := result.Snapshot.Fields["quantity"].Value; got != int64(200) {
		t.Fatalf("quantity = %#v, want 200", got)
	}
}

func TestMergeReportsMissingRequiredFields(t *testing.T) {
	result := Merge(
		DraftSnapshot{},
		FieldPatch{Fields: map[string]PatchValue{
			"product_sku": {Value: "SKU-C68", Source: SourceUser},
		}},
		MergePolicy{RequiredFields: []string{"product_sku", "quantity", "receive_address"}},
	)

	if result.Status != MergeStatusCurrent {
		t.Fatalf("status = %s, want CURRENT", result.Status)
	}
	if len(result.Missing) != 2 || result.Missing[0] != "quantity" || result.Missing[1] != "receive_address" {
		t.Fatalf("missing = %#v, want quantity and receive_address", result.Missing)
	}
}
