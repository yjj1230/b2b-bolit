package agentorder

import (
	"reflect"
	"sort"
)

// FieldSource identifies who supplied a value. Higher priority sources can
// replace lower priority values deterministically.
type FieldSource string

const (
	SourceModel     FieldSource = "MODEL"
	SourceConfirmed FieldSource = "CONFIRMED"
	SourceUser      FieldSource = "USER"
	SourceHuman     FieldSource = "HUMAN"
)

func sourcePriority(source FieldSource) int {
	switch source {
	case SourceHuman:
		return 4
	case SourceUser:
		return 3
	case SourceConfirmed:
		return 2
	case SourceModel:
		return 1
	default:
		return 0
	}
}

// FieldValue is the normalized value stored in a cumulative draft snapshot.
type FieldValue struct {
	Value    any         `json:"value"`
	Source   FieldSource `json:"source"`
	Evidence string      `json:"evidence,omitempty"`
}

// DraftSnapshot is the cumulative normalized order draft.
type DraftSnapshot struct {
	Fields map[string]FieldValue `json:"fields"`
}

// PatchValue is one field candidate extracted from the current message.
type PatchValue struct {
	Value               any         `json:"value"`
	Source              FieldSource `json:"source"`
	ExplicitReplacement bool        `json:"explicit_replacement,omitempty"`
	Evidence            string      `json:"evidence,omitempty"`
}

// FieldPatch contains only the current message's incremental fields.
type FieldPatch struct {
	Fields map[string]PatchValue `json:"fields"`
}

// MergePolicy controls deterministic merge checks.
type MergePolicy struct {
	RequiredFields []string
}

type MergeStatus string

const (
	MergeStatusCurrent  MergeStatus = "CURRENT"
	MergeStatusConflict MergeStatus = "CONFLICT"
)

type DiffKind string

const (
	DiffAdded     DiffKind = "ADDED"
	DiffChanged   DiffKind = "CHANGED"
	DiffUnchanged DiffKind = "UNCHANGED"
	DiffConflict  DiffKind = "CONFLICT"
)

type FieldDiff struct {
	Kind     DiffKind   `json:"kind"`
	Before   any        `json:"before,omitempty"`
	After    any        `json:"after,omitempty"`
	Source   FieldSource `json:"source,omitempty"`
	Evidence string     `json:"evidence,omitempty"`
}

type FieldConflict struct {
	Field    string      `json:"field"`
	Previous FieldValue  `json:"previous"`
	Incoming PatchValue  `json:"incoming"`
}

// MergeResult is deterministic and has no database, network, or model side effects.
type MergeResult struct {
	Snapshot  DraftSnapshot
	Patch     FieldPatch
	Diff      map[string]FieldDiff
	Conflicts []FieldConflict
	Missing   []string
	Status    MergeStatus
}

// Merge applies one incremental patch to a cumulative snapshot.
// Lower-priority or equally-prioritized conflicting values never silently
// replace an existing value; they produce a recoverable CONFLICT result.
func Merge(previous DraftSnapshot, patch FieldPatch, policy MergePolicy) MergeResult {
	out := MergeResult{
		Snapshot: DraftSnapshot{Fields: cloneFields(previous.Fields)},
		Patch:    FieldPatch{Fields: clonePatch(patch.Fields)},
		Diff:     map[string]FieldDiff{},
		Status:   MergeStatusCurrent,
	}
	if out.Snapshot.Fields == nil {
		out.Snapshot.Fields = map[string]FieldValue{}
	}

	keys := make([]string, 0, len(patch.Fields))
	for key := range patch.Fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, field := range keys {
		incoming := patch.Fields[field]
		existing, exists := out.Snapshot.Fields[field]
		if !exists || isEmpty(existing.Value) {
			out.Snapshot.Fields[field] = FieldValue{
				Value: incoming.Value, Source: incoming.Source, Evidence: incoming.Evidence,
			}
			out.Diff[field] = FieldDiff{Kind: DiffAdded, After: incoming.Value, Source: incoming.Source, Evidence: incoming.Evidence}
			continue
		}
		if reflect.DeepEqual(existing.Value, incoming.Value) {
			if sourcePriority(incoming.Source) > sourcePriority(existing.Source) && incoming.Evidence != "" {
				out.Snapshot.Fields[field] = FieldValue{Value: existing.Value, Source: incoming.Source, Evidence: incoming.Evidence}
			}
			out.Diff[field] = FieldDiff{Kind: DiffUnchanged, Before: existing.Value, After: existing.Value, Source: existing.Source}
			continue
		}

		shouldReplace := incoming.ExplicitReplacement || sourcePriority(incoming.Source) > sourcePriority(existing.Source)
		if shouldReplace {
			out.Snapshot.Fields[field] = FieldValue{
				Value: incoming.Value, Source: incoming.Source, Evidence: incoming.Evidence,
			}
			out.Diff[field] = FieldDiff{Kind: DiffChanged, Before: existing.Value, After: incoming.Value, Source: incoming.Source, Evidence: incoming.Evidence}
			continue
		}

		out.Status = MergeStatusConflict
		out.Conflicts = append(out.Conflicts, FieldConflict{Field: field, Previous: existing, Incoming: incoming})
		out.Diff[field] = FieldDiff{Kind: DiffConflict, Before: existing.Value, After: incoming.Value, Source: incoming.Source, Evidence: incoming.Evidence}
	}

	for _, field := range policy.RequiredFields {
		value, ok := out.Snapshot.Fields[field]
		if !ok || isEmpty(value.Value) {
			out.Missing = append(out.Missing, field)
		}
	}
	sort.Strings(out.Missing)
	return out
}

func cloneFields(in map[string]FieldValue) map[string]FieldValue {
	out := make(map[string]FieldValue, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func clonePatch(in map[string]PatchValue) map[string]PatchValue {
	out := make(map[string]PatchValue, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func isEmpty(value any) bool {
	if value == nil {
		return true
	}
	if s, ok := value.(string); ok {
		return s == ""
	}
	return false
}
