// Package evaluation evaluates LLM intent extraction against versioned JSONL
// golden cases. It intentionally stops before database writes and workflow
// actions; deterministic routing and approval are evaluated separately.
package evaluation

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
)

// Parser is the narrow contract needed by the evaluator. wecom.IntentParser
// satisfies it without creating orders or touching workflow state.
type Parser interface {
	Parse(context.Context, string, string, string, string) ([]byte, error)
}

type Turn struct {
	MsgID    string `json:"msg_id"`
	SenderID string `json:"sender_id,omitempty"`
	Text     string `json:"text"`
}

type Action struct {
	Type           string `json:"type"`
	Operator       string `json:"operator,omitempty"`
	DraftVersion   int64  `json:"draft_version,omitempty"`
	Quantity       int64  `json:"quantity,omitempty"`
	UnitPriceCents int64  `json:"unit_price_cents,omitempty"`
}

type Expected struct {
	Intent        string           `json:"intent"`
	Candidates    []map[string]any `json:"candidates"`
	Routing       string           `json:"routing"`
	MissingFields []string         `json:"missing_fields,omitempty"`
	Reason        string           `json:"reason,omitempty"`
	Revision      int64            `json:"revision,omitempty"`
	WorkflowState string           `json:"workflow_state,omitempty"`
	ContextFields map[string]any   `json:"context_fields,omitempty"`
}

type GoldenCase struct {
	ID        string   `json:"id"`
	Category  string   `json:"category"`
	Turns     []Turn   `json:"turns"`
	Actions   []Action `json:"actions,omitempty"`
	Expected  Expected `json:"expected"`
	Forbidden []string `json:"forbidden,omitempty"`
}

type Options struct {
	TenantID       string
	GroupID        string
	DatasetVersion string
	Model          string
	PromptVersion  string
}

type Report struct {
	DatasetVersion          string       `json:"dataset_version,omitempty"`
	Model                   string       `json:"model,omitempty"`
	PromptVersion           string       `json:"prompt_version,omitempty"`
	TotalCases              int          `json:"total_cases"`
	ValidJSONCases          int          `json:"valid_json_cases"`
	JSONValidRate           float64      `json:"json_valid_rate"`
	ExactCandidateCases     int          `json:"exact_candidate_cases"`
	ExactCandidateRate      float64      `json:"exact_candidate_rate"`
	ExpectedFieldCount      int          `json:"expected_field_count"`
	CorrectFieldCount       int          `json:"correct_field_count"`
	FieldAccuracy           float64      `json:"field_accuracy"`
	HallucinationViolations int          `json:"hallucination_violations"`
	HallucinationCaseRate   float64      `json:"hallucination_case_rate"`
	ParserErrorCases        int          `json:"parser_error_cases"`
	ParserErrorRate         float64      `json:"parser_error_rate"`
	MultiTurnCases          int          `json:"multi_turn_cases"`
	RoutingEvaluatedCases   int          `json:"routing_evaluated_cases"`
	Cases                   []CaseResult `json:"cases"`
}

type CaseResult struct {
	ID                      string           `json:"id"`
	Category                string           `json:"category"`
	ValidJSON               bool             `json:"valid_json"`
	ExactCandidates         bool             `json:"exact_candidates"`
	ExpectedFieldCount      int              `json:"expected_field_count"`
	CorrectFieldCount       int              `json:"correct_field_count"`
	HallucinationViolations int              `json:"hallucination_violations"`
	ActualCandidates        []map[string]any `json:"actual_candidates,omitempty"`
	Error                   string           `json:"error,omitempty"`
}

// LoadJSONL parses a golden dataset. Blank lines are ignored; malformed lines
// return an error with the source line number so CI can reject a bad fixture.
func LoadJSONL(r io.Reader) ([]GoldenCase, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	var cases []GoldenCase
	seenIDs := make(map[string]struct{})
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var c GoldenCase
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("golden dataset line %d: %w", lineNo, err)
		}
		if c.ID == "" {
			return nil, fmt.Errorf("golden dataset line %d: id is required", lineNo)
		}
		if _, exists := seenIDs[c.ID]; exists {
			return nil, fmt.Errorf("golden dataset line %d: duplicate case id %q", lineNo, c.ID)
		}
		seenIDs[c.ID] = struct{}{}
		if len(c.Turns) == 0 {
			return nil, fmt.Errorf("golden dataset line %d (%s): turns are required", lineNo, c.ID)
		}
		cases = append(cases, c)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read golden dataset: %w", err)
	}
	return cases, nil
}

// Evaluate runs parser-only extraction evaluation. Routing and workflow fields
// are retained in the golden cases for the L3 replay runner and are not
// counted here because the parser returns candidates, not a business decision.
func Evaluate(ctx context.Context, parser Parser, cases []GoldenCase, opts Options) (Report, error) {
	if parser == nil {
		return Report{}, fmt.Errorf("evaluation parser is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.TenantID == "" {
		opts.TenantID = "evaluation"
	}
	if opts.GroupID == "" {
		opts.GroupID = "golden"
	}
	report := Report{
		DatasetVersion: opts.DatasetVersion,
		Model:          opts.Model,
		PromptVersion:  opts.PromptVersion,
		TotalCases:     len(cases),
		Cases:          make([]CaseResult, 0, len(cases)),
	}
	for _, golden := range cases {
		result := CaseResult{ID: golden.ID, Category: golden.Category}
		if len(golden.Turns) > 1 {
			report.MultiTurnCases++
		}
		input := aggregateTurns(golden.Turns)
		raw, err := parser.Parse(ctx, opts.TenantID, opts.GroupID, golden.ID, input)
		if err != nil {
			result.Error = err.Error()
			report.ParserErrorCases++
			report.Cases = append(report.Cases, result)
			continue
		}
		actual, err := decodeCandidates(raw)
		if err != nil {
			result.Error = err.Error()
			report.ParserErrorCases++
			report.Cases = append(report.Cases, result)
			continue
		}
		result.ValidJSON = true
		report.ValidJSONCases++
		result.ExpectedFieldCount, result.CorrectFieldCount, result.ExactCandidates = compareCandidates(golden.Expected.Candidates, actual)
		result.HallucinationViolations = countHallucinations(golden, actual)
		report.ExpectedFieldCount += result.ExpectedFieldCount
		report.CorrectFieldCount += result.CorrectFieldCount
		report.HallucinationViolations += result.HallucinationViolations
		result.ActualCandidates = actual
		report.Cases = append(report.Cases, result)
		if result.ExactCandidates {
			report.ExactCandidateCases++
		}
	}
	if report.ExpectedFieldCount > 0 {
		report.FieldAccuracy = float64(report.CorrectFieldCount) / float64(report.ExpectedFieldCount)
	}
	if report.TotalCases > 0 {
		report.JSONValidRate = float64(report.ValidJSONCases) / float64(report.TotalCases)
		report.ExactCandidateRate = float64(report.ExactCandidateCases) / float64(report.TotalCases)
		report.HallucinationCaseRate = float64(countCasesWithHallucinations(report.Cases)) / float64(report.TotalCases)
		report.ParserErrorRate = float64(report.ParserErrorCases) / float64(report.TotalCases)
	}
	return report, nil
}

func countCasesWithHallucinations(cases []CaseResult) int {
	count := 0
	for _, result := range cases {
		if result.HallucinationViolations > 0 {
			count++
		}
	}
	return count
}

func aggregateTurns(turns []Turn) string {
	if len(turns) == 1 {
		return turns[0].Text
	}
	parts := make([]string, 0, len(turns))
	for i, turn := range turns {
		parts = append(parts, fmt.Sprintf("[turn %d] %s", i+1, turn.Text))
	}
	return strings.Join(parts, "\n")
}

func decodeCandidates(raw []byte) ([]map[string]any, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || !strings.HasPrefix(trimmed, "[") {
		return nil, fmt.Errorf("candidate response must be a JSON array")
	}
	var candidates []map[string]any
	if err := json.Unmarshal(raw, &candidates); err != nil {
		return nil, fmt.Errorf("decode candidate array: %w", err)
	}
	if candidates == nil {
		return nil, fmt.Errorf("candidate response must be a JSON array")
	}
	return candidates, nil
}

func compareCandidates(expected, actual []map[string]any) (expectedFields, correctFields int, exact bool) {
	expectedFields = countFields(expected)
	if len(expected) != len(actual) {
		return expectedFields, compareMatchedFields(expected, actual), false
	}
	used := make([]bool, len(actual))
	exact = true
	for i, want := range expected {
		idx := findCandidate(want, actual, used, i)
		if idx < 0 {
			exact = false
			continue
		}
		used[idx] = true
		correct, fields := compareCandidateFields(want, actual[idx])
		correctFields += correct
		if fields != correct || hasMeaningfulUnexpectedFields(want, actual[idx]) {
			exact = false
		}
	}
	return expectedFields, correctFields, exact
}

func compareMatchedFields(expected, actual []map[string]any) int {
	used := make([]bool, len(actual))
	correct := 0
	for i, want := range expected {
		idx := findCandidate(want, actual, used, i)
		if idx < 0 {
			continue
		}
		used[idx] = true
		matched, _ := compareCandidateFields(want, actual[idx])
		correct += matched
	}
	return correct
}

func countFields(candidates []map[string]any) int {
	total := 0
	for _, candidate := range candidates {
		for key := range candidate {
			if key != "confidence" {
				total++
			}
		}
	}
	return total
}

func findCandidate(want map[string]any, actual []map[string]any, used []bool, fallback int) int {
	if sku, ok := want["product_sku"].(string); ok && sku != "" {
		for i, candidate := range actual {
			if !used[i] && candidateString(candidate, "product_sku") == sku {
				return i
			}
		}
	}
	if fallback < len(actual) && !used[fallback] {
		return fallback
	}
	for i := range actual {
		if !used[i] {
			return i
		}
	}
	return -1
}

func compareCandidateFields(expected, actual map[string]any) (correct, fields int) {
	for key, want := range expected {
		if key == "confidence" {
			continue
		}
		fields++
		if got, ok := actual[key]; ok && equalJSONValue(want, got) {
			correct++
		}
	}
	return correct, fields
}

func hasMeaningfulUnexpectedFields(expected, actual map[string]any) bool {
	for key, value := range actual {
		if key == "confidence" || valueIsEmpty(value) {
			continue
		}
		if _, exists := expected[key]; !exists {
			return true
		}
	}
	return false
}

func countHallucinations(golden GoldenCase, actual []map[string]any) int {
	violations := 0
	if len(golden.Expected.Candidates) == 0 && len(actual) > 0 &&
		(golden.Expected.Intent == "OTHER" || golden.Expected.Routing == "IGNORE") {
		violations++
	}
	for _, forbidden := range golden.Forbidden {
		var found bool
		switch forbidden {
		case "invented_product_id":
			found = anyMeaningful(actual, "product_id")
		case "invented_customer_id":
			found = anyMeaningful(actual, "customer_id")
		case "invented_price":
			found = anyMeaningful(actual, "unit_price_cents")
		case "invented_receive_address":
			found = anyMeaningful(actual, "receive_address")
		case "guessed_product_sku":
			found = anyMeaningful(actual, "product_sku")
		case "default_quantity":
			found = anyMeaningful(actual, "quantity")
		}
		if found {
			violations++
		}
	}
	return violations
}

func anyMeaningful(candidates []map[string]any, key string) bool {
	for _, candidate := range candidates {
		if value, ok := candidate[key]; ok && !valueIsEmpty(value) {
			return true
		}
	}
	return false
}

func candidateString(candidate map[string]any, key string) string {
	value, _ := candidate[key].(string)
	return value
}

func valueIsEmpty(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(v) == ""
	case float64:
		return v == 0
	case float32:
		return v == 0
	case int:
		return v == 0
	case int64:
		return v == 0
	case json.Number:
		f, err := v.Float64()
		return err == nil && f == 0
	default:
		return false
	}
}

func equalJSONValue(left, right any) bool {
	lf, lok := numberValue(left)
	rf, rok := numberValue(right)
	if lok || rok {
		return lok && rok && math.Abs(lf-rf) < 1e-9
	}
	return fmt.Sprint(left) == fmt.Sprint(right)
}

func numberValue(value any) (float64, bool) {
	switch v := value.(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case int32:
		return float64(v), true
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}
