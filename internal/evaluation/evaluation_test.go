package evaluation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeParser struct {
	responses []string
	err       error
	inputs    []string
}

func (p *fakeParser) Parse(_ context.Context, _, _, _, text string) ([]byte, error) {
	p.inputs = append(p.inputs, text)
	if p.err != nil {
		return nil, p.err
	}
	response := p.responses[0]
	p.responses = p.responses[1:]
	return []byte(response), nil
}

func TestLoadJSONLReadsGoldenCases(t *testing.T) {
	cases, err := LoadJSONL(strings.NewReader(`
{"id":"a","category":"single_turn","turns":[{"msg_id":"m1","text":"C68 10件"}],"expected":{"intent":"CREATE_ORDER","candidates":[{"product_sku":"SKU-C68","quantity":10}],"routing":"PENDING_CONFIRM"}}
{"id":"b","category":"negative","turns":[{"msg_id":"m2","text":"天气不错"}],"expected":{"intent":"OTHER","candidates":[],"routing":"IGNORE"}}
`))
	require.NoError(t, err)
	require.Len(t, cases, 2)
	require.Equal(t, "a", cases[0].ID)
	require.EqualValues(t, int64(10), cases[0].Expected.Candidates[0]["quantity"])
}

func TestLoadJSONLRejectsDuplicateCaseIDs(t *testing.T) {
	_, err := LoadJSONL(strings.NewReader(`
{"id":"duplicate","category":"single_turn","turns":[{"msg_id":"m1","text":"C68 10件"}],"expected":{"intent":"CREATE_ORDER"}}
{"id":"duplicate","category":"single_turn","turns":[{"msg_id":"m2","text":"D90 5台"}],"expected":{"intent":"CREATE_ORDER"}}
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate")
}

func TestEvaluateScoresFieldsAndHallucinations(t *testing.T) {
	parser := &fakeParser{responses: []string{
		`[{"product_sku":"SKU-C68","quantity":10}]`,
		`[{"product_sku":"SKU-D90"}]`,
		`[{"product_sku":"SKU-C68","product_id":"invented","unit_price_cents":999}]`,
	}}
	cases := []GoldenCase{
		{ID: "exact", Category: "single_turn", Turns: []Turn{{MsgID: "m1", Text: "C68 10件"}}, Expected: Expected{Intent: "CREATE_ORDER", Candidates: []map[string]any{{"product_sku": "SKU-C68", "quantity": int64(10)}}, Routing: "PENDING_CONFIRM"}},
		{ID: "partial", Category: "missing_field", Turns: []Turn{{MsgID: "m2", Text: "D90"}}, Expected: Expected{Intent: "CREATE_ORDER", Candidates: []map[string]any{{"product_sku": "SKU-D90", "quantity": int64(5)}}, Routing: "AMBIGUOUS"}},
		{ID: "negative", Category: "negative", Turns: []Turn{{MsgID: "m3", Text: "C68"}}, Expected: Expected{Intent: "OTHER", Candidates: nil, Routing: "IGNORE"}, Forbidden: []string{"invented_product_id", "invented_price"}},
	}
	report, err := Evaluate(context.Background(), parser, cases, Options{DatasetVersion: "test-v1", Model: "test-model", PromptVersion: "test-prompt"})
	require.NoError(t, err)
	require.Equal(t, 3, report.TotalCases)
	require.Equal(t, 3, report.ValidJSONCases)
	require.Equal(t, 1, report.ExactCandidateCases)
	require.Equal(t, 4, report.ExpectedFieldCount)
	require.Equal(t, 3, report.CorrectFieldCount)
	require.Equal(t, 3, report.HallucinationViolations)
	require.Equal(t, 0, report.ParserErrorCases)
	require.Len(t, report.Cases, 3)
	require.Equal(t, "partial", report.Cases[1].ID)
	require.False(t, report.Cases[1].ExactCandidates)
}

func TestEvaluateAggregatesMultiTurnInputInOrder(t *testing.T) {
	parser := &fakeParser{responses: []string{`[]`}}
	cases := []GoldenCase{{
		ID:       "multi",
		Category: "multi_turn",
		Turns:    []Turn{{MsgID: "m1", Text: "客户要 C68"}, {MsgID: "m2", Text: "数量 10 件"}},
		Expected: Expected{Intent: "CREATE_ORDER", Routing: "AMBIGUOUS"},
	}}
	_, err := Evaluate(context.Background(), parser, cases, Options{})
	require.NoError(t, err)
	require.Len(t, parser.inputs, 1)
	require.Equal(t, "[turn 1] 客户要 C68\n[turn 2] 数量 10 件", parser.inputs[0])
}

func TestEvaluateRecordsParserErrorsAndContinues(t *testing.T) {
	parser := &fakeParser{err: errors.New("gateway unavailable")}
	cases := []GoldenCase{{ID: "failed", Turns: []Turn{{MsgID: "m1", Text: "C68 10件"}}, Expected: Expected{Intent: "CREATE_ORDER"}}}
	report, err := Evaluate(context.Background(), parser, cases, Options{})
	require.NoError(t, err)
	require.Equal(t, 1, report.ParserErrorCases)
	require.Contains(t, report.Cases[0].Error, "gateway unavailable")
}
