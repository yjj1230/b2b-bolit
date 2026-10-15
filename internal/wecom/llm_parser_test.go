package wecom

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLLMParserAcceptsCandidateArrayAndNormalizesJSON(t *testing.T) {
	p := NewLLMParserWithComplete(func(context.Context, string, string, string, string) ([]byte, error) {
		return []byte(`[{"product_sku":"SKU-C68","quantity":10,"confidence":0.91}]`), nil
	}, "gpt-5.6", "gpt-5.6", "v1")

	got, err := p.Parse(context.Background(), "tenant", "group", "msg", "SKU-C68 10件")
	require.NoError(t, err)
	require.Equal(t, `[{"product_sku":"SKU-C68","product_id":"","quantity":10,"unit_price_cents":0,"confidence":0.91}]`, string(got))
}

func TestLLMParserRejectsObjectAndMarkdownResponses(t *testing.T) {
	for _, raw := range []string{
		`{"candidates":[]}`,
		"```json\n[]\n```",
		`[] {"unexpected":"trailing"}`,
		`[{"product_sku":"SKU-C68","quantity":1,"unexpected":true}]`,
	} {
		p := NewLLMParserWithComplete(func(context.Context, string, string, string, string) ([]byte, error) {
			return []byte(raw), nil
		}, "gpt-5.6", "gpt-5.6", "v1")
		_, err := p.Parse(context.Background(), "tenant", "group", "msg", "text")
		require.Error(t, err, "raw=%q", raw)
		require.True(t, strings.Contains(err.Error(), "llm intent schema"), "err=%v", err)
	}
}
