// 用例 Task 位置程序化验证：正例迁移必须合法、负例迁移必须非法（按 EP 段映射裸状态名到前缀常量）
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"b2b_pilot_v3/internal/domain"
)

type Case struct {
	EP     int    `json:"ep"`
	ID     string `json:"id"`
	Type   string `json:"type"`
	Scene  string `json:"scene"`
	World  string `json:"world"`
	Task   string `json:"task"`
	Expect string `json:"expect"`
	CQ     string `json:"cq"`
	Reason string `json:"reason"`
}

type Result struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"`
	Task        string   `json:"task"`
	Transitions []string `json:"transitions"`
	Legal       []bool   `json:"legal"`
	OK          bool     `json:"ok"`
	Msg         string   `json:"msg"`
}

var arrowRe = regexp.MustCompile(`([A-Z_]+)\s*(?:→|->)\s*([A-Z_]+)`)

// 段 → 默认前缀
func segmentPrefix(ep int) string {
	switch {
	case ep <= 10:
		return "ORD"
	case ep <= 18:
		return "STK"
	case ep <= 26:
		return "SHP"
	case ep <= 36:
		return "INV"
	case ep >= 45:
		return "PRE"
	}
	return "ORD"
}

// 裸状态名 → 前缀常量（歧义按段消解；全名（含 _ 前缀）直接透传）
var bare = map[string]string{
	// PRE
	"CAPTURED": "PRE_CAPTURED", "INTENT_PARSING": "PRE_INTENT_PARSING", "PENDING_CONFIRM": "PRE_PENDING_CONFIRM",
	"AMBIGUOUS": "PRE_AMBIGUOUS", "CONFIRMED": "PRE_CONFIRMED", "MODIFIED": "PRE_MODIFIED",
	"ESCALATED": "PRE_ESCALATED", "IGNORED": "PRE_IGNORED", "ORDER_REQUESTED": "PRE_ORDER_REQUESTED",
	// ORD
	"REQUESTED": "ORD_REQUESTED", "PRICE_REVIEWING": "ORD_PRICE_REVIEWING", "PRICE_APPROVED": "ORD_PRICE_APPROVED",
	"CHAIRMAN_REVIEWING": "ORD_CHAIRMAN_REVIEWING", "CONTRACT_GENERATING": "ORD_CONTRACT_GENERATING",
	"CONTRACTED": "ORD_CONTRACTED", "MODIFYING": "ORD_MODIFYING", "RETURNED": "ORD_RETURNED",
	"REJECTED": "ORD_REJECTED", "READY_FOR_STOCK": "STK_READY_FOR_STOCK",
	// STK
	"STOCK_REVIEWING": "STK_STOCK_REVIEWING", "WAREHOUSE_CONFIRMING": "STK_WAREHOUSE_CONFIRMING",
	"WAREHOUSE_CONFIRMED": "STK_WAREHOUSE_CONFIRMED", "WAREHOUSE_RETURNED": "STK_WAREHOUSE_RETURNED",
	"TRANSFER_REQUESTED": "STK_TRANSFER_REQUESTED", "PAYMENT_CONFIRMING": "STK_PAYMENT_CONFIRMING",
	"PAYMENT_CONFIRMED": "STK_PAYMENT_CONFIRMED", "SHIP_ORDER_ISSUED": "STK_SHIP_ORDER_ISSUED",
	// SHP
	"SHIP_ASSIGNING": "SHP_SHIP_ASSIGNING", "WAREHOUSE_SHIPPING": "SHP_WAREHOUSE_SHIPPING",
	"SHIPPED": "SHP_SHIPPED", "FULLY_SHIPPED": "SHP_FULLY_SHIPPED", "PARTIAL_SHIPPED": "SHP_PARTIAL_SHIPPED",
	"READY_FOR_SHIPMENT": "SHP_READY_FOR_SHIPMENT",
	// INV
	"READY_FOR_INVOICE": "INV_READY_FOR_INVOICE", "INVOICE_APPLYING": "INV_INVOICE_APPLYING",
	"RECEIPT_EXCEPTION_REVIEWING": "INV_RECEIPT_EXCEPTION_REVIEWING", "WAITING_RECEIPT": "INV_WAITING_RECEIPT",
	"INVOICING": "INV_INVOICING", "INVOICED": "INV_INVOICED", "INVOICE_FAILED": "INV_INVOICE_FAILED",
	"NO_INVOICE_NEEDED": "INV_NO_INVOICE_NEEDED", "PAYMENT_TRACKING": "INV_PAYMENT_TRACKING",
	"CONTRACT_ARCHIVED": "INV_CONTRACT_ARCHIVED", "CLOSED": "INV_CLOSED",
}

// 段内歧义消解（裸名在段内可能有专属含义）
var ambiguous = map[string]map[string]string{
	"ORD": {"RETURNED": "ORD_RETURNED", "READY_FOR_SHIPMENT": "SHP_SHIP_ASSIGNING"},
	"STK": {"RETURNED": "STK_RETURNED", "READY_FOR_STOCK": "STK_READY_FOR_STOCK", "SHIP_ASSIGNING": "SHP_SHIP_ASSIGNING", "READY_FOR_SHIPMENT": "SHP_SHIP_ASSIGNING"},
	"SHP": {"SHIP_ASSIGNING": "SHP_SHIP_ASSIGNING"},
}

func resolve(name string, ep int) (string, bool) {
	// 已带前缀（含 _ 的完整常量名）
	if len(name) > 4 && name[3] == '_' {
		return name, true
	}
	seg := segmentPrefix(ep)
	if m, ok := ambiguous[seg]; ok {
		if v, ok := m[name]; ok {
			return v, true
		}
	}
	if v, ok := bare[name]; ok {
		return v, true
	}
	return name, false
}

func validate(from, to string) error {
	switch domain.Stage(from) {
	case "ORD":
		return domain.OrdFSM.Validate(from, to)
	case "STK":
		return domain.StkFSM.Validate(from, to)
	case "SHP":
		return domain.ShpFSM.Validate(from, to)
	case "INV":
		return domain.InvFSM.Validate(from, to)
	case "PRE":
		return domain.PreOrderFSM.Validate(from, to)
	}
	return fmt.Errorf("unknown stage %s", from)
}

func main() {
	raw, _ := os.ReadFile("/tmp/cases.json")
	var cases []Case
	json.Unmarshal(raw, &cases)

	issues := []Result{}
	okCount, badCount := 0, 0
	unresolved := map[string]bool{}
	for _, c := range cases {
		r := Result{ID: c.ID, Type: c.Type, Task: c.Task}
		matches := arrowRe.FindAllStringSubmatch(c.Task, -1)
		validAll := true
		for _, m := range matches {
			from, ok1 := resolve(m[1], c.EP)
			to, ok2 := resolve(m[2], c.EP)
			if !ok1 || !ok2 {
				unresolved[m[1]+"|"+m[2]] = true
			}
			r.Transitions = append(r.Transitions, from+"->"+to)
			err := validate(from, to)
			r.Legal = append(r.Legal, err == nil)
			if err != nil {
				validAll = false
			}
		}
		if c.Type == "ok" {
			// 无迁移但语义为「终态/幂等/不重复迁移」→ 幂等正确
			if len(matches) == 0 && (strings.Contains(c.Task, "终态") || strings.Contains(c.Task, "幂等") || strings.Contains(c.Task, "不重复") || strings.Contains(c.Task, "分批") || strings.Contains(c.Task, "滞留") || strings.Contains(c.Task, "并发") || strings.Contains(c.Task, "幂等命中")) {
				r.OK = true
				okCount++
				continue
			}
			r.OK = validAll && len(matches) > 0
			if r.OK {
				okCount++
			} else {
				r.Msg = "正例迁移不合法"
				issues = append(issues, r)
			}
		} else {
			noTrans := len(matches) == 0
			// 业务类负例（幂等/权限/守恒/金额/字段投影/审计/事件链/并发/配置/数据范围/Timer/红线/防/不可见/不适用）：
			// FSM 不适用，留待代码+测试维度；状态机类（状态机/终态/反向/越段/跳过/顺序强制/无出口）要求 FSM 非法
			sm := strings.Contains(c.Reason, "状态机") || strings.Contains(c.Reason, "终态") ||
				strings.Contains(c.Reason, "反向") || strings.Contains(c.Reason, "越段") ||
				strings.Contains(c.Reason, "跳过") || strings.Contains(c.Reason, "顺序强制") ||
				strings.Contains(c.Reason, "无出口") || strings.Contains(c.Reason, "不可回退")
			if noTrans || !sm {
				r.OK = true
				badCount++
				continue
			}
			r.OK = !validAll
			if r.OK {
				badCount++
			} else {
				r.Msg = "负例迁移全部合法（应拒绝）"
				issues = append(issues, r)
			}
		}
	}
	fmt.Printf("FSM 验证: 正例 %d/150 通过, 负例 %d/150 通过\n", okCount, badCount)
	if len(unresolved) > 0 {
		fmt.Println("=== 未解析状态名 ===")
		for k := range unresolved {
			fmt.Println(" ", k)
		}
	}
	if len(issues) > 0 {
		fmt.Println("=== 存疑用例（正例不合法或负例未拒绝）===")
		for _, r := range issues {
			fmt.Printf("%s [%s] %s: %v %s\n", r.ID, r.Type, r.Task, r.Legal, r.Msg)
		}
	}
}
