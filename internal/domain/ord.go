package domain

import (
	"fmt"
	"strings"
)

// OrdFSM —— ORD 订单创建流程状态机（docs/01-core-graph.html §2.2）。
var OrdFSM = NewFSM("ORD", "", map[string][]string{
	StateORDRequested: {
		StateORDPriceReviewing, // 基础校验+客户等级校验通过
		StateORDRejected,       // ❌ 校验失败
	},
	StateORDPriceReviewing: {
		StateORDPriceApproved, // ✅ 销售主管通过（Signal）
		StateORDReturned,      // 退回修改（Signal）
		StateORDRejected,      // 拒绝终止（Signal）
	},
	StateORDPriceApproved: {
		StateORDChairmanReview,   // 客户等级 C/D
		StateORDContractGenerate, // 客户等级 A/B（跳过董事长）
	},
	StateORDChairmanReview: {
		StateORDContractGenerate, // ✅ 董事长通过（Signal）
		StateORDRejected,         // ❌ 董事长拒绝
		StateORDReturned,         // DSZ-01：董事长要求修改（Signal RETURNED）
	},
	StateORDContractGenerate: {
		StateORDContracted,       // 合同生成校验通过
		StateORDContractGenerate, // 要素不全→补全重校验（自循环）
	},
	StateORDContracted: {
		StateSTKReadyForStock, // 进入备货段
		StateORDModifying,     // 修改请求（A/B/C/D/E 分类）
	},
	StateORDModifying: {
		StateORDPriceReviewing,   // A 类：回待价格审批重审
		StateSHPShipAssigning,    // B 类：账期改非款到→直达发货阶段
		StateORDContractGenerate, // C 类：仅重新生成合同
		StateSHPShipAssigning,    // D 类：重生成合同+跳过备货财务→直达发货
		StateORDContracted,       // E 类：静默修改（仅更新 payload+事件）
	},
	StateORDReturned: {
		StateORDPriceReviewing, // 审价退回后重提
		StateORDPriceApproved,  // DSZ-01：董事长要求修改后重提 → 重新董事长终审
	},
})

// ── 修改分类（A/B/C/D/E，规则为配置数据，docs/01-core-graph.html §2.2 分类规则表）──

// ModificationClass 修改分类。
type ModificationClass string

const (
	ModClassA ModificationClass = "A" // 客户名称/产品型号及单价/客户等级 → 回审价
	ModClassB ModificationClass = "B" // 修改包含账期 → 非款到直接进发货
	ModClassC ModificationClass = "C" // 修改包含供方 → 仅重新生成合同
	ModClassD ModificationClass = "D" // 收货地址/产品型号/产品数量 → 重生成合同+直达发货
	ModClassE ModificationClass = "E" // 其余 → 静默修改
)

// Modification 描述一次订单修改请求的变更项。
type Modification struct {
	CustomerNameChanged    bool // A
	ProductOrPriceChanged  bool // A
	GradeChanged           bool // A
	PaymentTermChanged     bool // B
	SupplierChannelChanged bool // C
	AddressChanged         bool // D
	ProductChanged         bool // D
	QuantityChanged        bool // D
}

// ClassifyModification 按规则表判定修改分类（A 优先于 B/C/D，B/C/D 优先于 E）。
func ClassifyModification(m Modification) ModificationClass {
	if m.CustomerNameChanged || m.ProductOrPriceChanged || m.GradeChanged {
		return ModClassA
	}
	if m.PaymentTermChanged {
		return ModClassB
	}
	if m.SupplierChannelChanged {
		return ModClassC
	}
	if m.AddressChanged || m.ProductChanged || m.QuantityChanged {
		return ModClassD
	}
	return ModClassE
}

// BlockShippedAddressChange D-03：已发货/运输中/开票后的订单禁止直接修改收货地址
// （最新 demo XS4：BLOCK 转物流异常，不修改主订单）。产品语义：地址改签走承运商改派。
func BlockShippedAddressChange(state string, class ModificationClass, changes []string) bool {
	if class != ModClassD {
		return false
	}
	addressChange := false
	for _, c := range changes {
		l := strings.ToLower(c)
		if strings.Contains(l, "address") || strings.Contains(l, "receive") || strings.Contains(l, "地址") {
			addressChange = true
			break
		}
	}
	if !addressChange {
		return false
	}
	switch {
	case state == StateSHPWarehouseShip, state == StateSHPShipPed,
		state == StateSHPFullyShipped, state == StateSHPPartialShipped:
		return true
	case strings.HasPrefix(state, "INV_"):
		return true
	default:
		return false
	}
}

// ModifyTargetState 返回修改分类对应的目标状态（与 OrdFSM 迁移表一致）。
func ModifyTargetState(class ModificationClass) (string, error) {
	switch class {
	case ModClassA:
		return StateORDPriceReviewing, nil
	case ModClassB, ModClassD:
		return StateSHPShipAssigning, nil
	case ModClassC:
		return StateORDContractGenerate, nil
	case ModClassE:
		return StateORDContracted, nil
	default:
		return "", fmt.Errorf("unknown modification class %q", class)
	}
}
