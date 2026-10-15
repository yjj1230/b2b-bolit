//go:build deferred

package wecom

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// 意图解析测试（暂缓：07 §3.0「基于意图识别的企微自动下单」暂缓实现、暂缓测试）。
// 默认构建不编译本文件（-tags deferred 下可运行，供恢复阶段回归）。
func TestMockIntentParser(t *testing.T) {
	p := NewMockParser()
	ctx := context.Background()

	// 命中 SKU + 数量
	b, err := p.Parse(ctx, "chang-an", "g1", "m1", "客户要 SKU-C68 10件")
	require.NoError(t, err)
	var cands []struct {
		ProductSKU string  `json:"product_sku"`
		Quantity   int64   `json:"quantity"`
		Confidence float64 `json:"confidence"`
	}
	require.NoError(t, json.Unmarshal(b, &cands))
	require.Len(t, cands, 1)
	require.Equal(t, "SKU-C68", cands[0].ProductSKU)
	require.Equal(t, int64(10), cands[0].Quantity)

	// 多 SKU → 多候选（低置信路径 → AMBIGUOUS）
	b2, err := p.Parse(ctx, "chang-an", "g1", "m2", "SKU-C68 5件 和 SKU-D90 3台")
	require.NoError(t, err)
	var cands2 []struct {
		ProductSKU string `json:"product_sku"`
	}
	require.NoError(t, json.Unmarshal(b2, &cands2))
	require.Len(t, cands2, 2)

	// 无 SKU → 空候选（不自动建单）
	b3, err := p.Parse(ctx, "chang-an", "g1", "m3", "随便聊聊")
	require.NoError(t, err)
	require.Equal(t, "[]", string(b3))

	// LLM 钩子未接线 → 明确报错（防静默降级）
	_, err = NewLLMParser().Parse(ctx, "chang-an", "g1", "m4", "x")
	require.Error(t, err)
	require.Contains(t, errLLMNotConfigured.Error(), "LLM")
}
