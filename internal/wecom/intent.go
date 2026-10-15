package wecom

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"b2b_pilot_v3/internal/workflows"
)

// ── 意图识别（07 §3：LLM 只产候选 + 确定性校验；AI 安全边界）──
//
// 生产 Parser 只生成结构化候选；正式订单仍必须经过确定性策略和人工确认。
// MockParser 仅用于测试注入，LLMParser 通过 Complete 适配器接入供应商，不直接写数据库。

// IntentParser 意图解析接口（mock 规则 / LLM 实现二选一；LLM 只产候选，最终裁决在
// workflows.ParseIntent 的确定性校验）。
type IntentParser interface {
	// Parse 从原始消息文本产出候选（结构化 JSON）。
	Parse(ctx context.Context, tenantID, groupID, msgID, text string) ([]byte, error)
}

// MockParser 规则解析器（mock 通道默认；keyword → SKU/数量）。
type MockParser struct {
	// Products SKU 映射表（标准型号归一化，07 §3.2；维护人待确认）
	Products map[string]string // sku → product_id
	Prices   map[string]int64  // sku → unit_price_cents（客户等级价格表简化）
}

// NewMockParser 构造默认产品表（长安仁恒演示数据）。
func NewMockParser() *MockParser {
	return &MockParser{
		Products: map[string]string{
			"SKU-C68": "prod-c68",
			"SKU-D90": "prod-d90",
		},
		Prices: map[string]int64{
			"SKU-C68": 1000,
			"SKU-D90": 2000,
		},
	}
}

var qtyRe = regexp.MustCompile(`(\d+)\s*件|(\d+)\s*台|(\d+)\s*个`)

// Parse 规则解析：文本中出现 SKU 且含数量 → 候选（高置信）；
// 无 SKU 或歧义 → 空候选（低置信 → AMBIGUOUS，不自动建单）。
func (p *MockParser) Parse(ctx context.Context, tenantID, groupID, msgID, text string) ([]byte, error) {
	var cands []workflows.IntentCandidate
	for sku, productID := range p.Products {
		if strings.Contains(text, sku) {
			qty := int64(1)
			if m := qtyRe.FindStringSubmatch(text); m != nil {
				for _, g := range m[1:] {
					if g != "" {
						if v, err := strconv.ParseInt(g, 10, 64); err == nil {
							qty = v
						}
					}
				}
			}
			cands = append(cands, workflows.IntentCandidate{
				ProductSKU: sku, ProductID: productID,
				Quantity: qty, UnitPriceCents: p.Prices[sku], Confidence: 0.95,
				// MockParser 仅用于本地测试注入；生产 Parser 必须从租户主数据填充这些字段。
				CustomerID: "cust-0001", PaymentTerm: "款到", SupplierChannel: "渠道A", WarehouseID: "wh-yangyuan",
			})
		}
	}
	if cands == nil {
		return []byte("[]"), nil // 空候选（低置信 → AMBIGUOUS，不自动建单）
	}
	return marshal(cands), nil
}

// LLMParser LLM 适配器。供应商调用通过 Complete 注入，解析器自身不写数据库。
type LLMParser struct {
	Complete      func(context.Context, string, string, string, string) ([]byte, error)
	ModelName     string
	ModelVersion  string
	PromptVersion string
}

// NewLLMParser 构造（当前返回未配置错误，防止静默降级）。
func NewLLMParser() *LLMParser { return &LLMParser{} }

func NewLLMParserWithComplete(complete func(context.Context, string, string, string, string) ([]byte, error), modelName, modelVersion, promptVersion string) *LLMParser {
	return &LLMParser{Complete: complete, ModelName: modelName, ModelVersion: modelVersion, PromptVersion: promptVersion}
}

// Parse 未接线实现。
func (p *LLMParser) Parse(ctx context.Context, tenantID, groupID, msgID, text string) ([]byte, error) {
	if p == nil || p.Complete == nil {
		return nil, errLLMNotConfigured
	}
	raw, err := p.Complete(ctx, tenantID, groupID, msgID, text)
	if err != nil {
		return nil, err
	}
	var cands []workflows.IntentCandidate
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cands); err != nil {
		return nil, fmt.Errorf("llm intent schema: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("llm intent schema: trailing JSON value")
		}
		return nil, fmt.Errorf("llm intent schema: trailing data: %w", err)
	}
	if cands == nil {
		return nil, fmt.Errorf("llm intent schema: candidates must be an array")
	}
	return marshal(cands), nil
}

// errLLMNotConfigured LLM 未接线错误。
type llmNotConfiguredError struct{}

func (e *llmNotConfiguredError) Error() string {
	return "LLM intent parser 未接线（基线用 MockParser）"
}

var errLLMNotConfigured = &llmNotConfiguredError{}
