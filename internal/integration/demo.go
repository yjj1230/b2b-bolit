// demo.go —— 本地演示/测试模式的业务配置策略（08 §5 接入层契约的本地变体）。
//
// 背景：api.LocalRunner 内嵌 Temporal TestSuite 环境，其虚拟时钟在工作流阻塞时会
// 自动触发最短 Timer（auto-fire），催办循环以 ~100µs/轮 烧穿 10 年运行预算
// （~0.4s/闸门），交互式演示（秒级点击）与慢信号测试会触发 workflow 执行超时。
//
// 解法：本地模式用 10s 虚拟时长替代生产 24h/48h/5d——每轮催办仅推进 10s 虚拟时间，
// 持续等待约 52 分钟才烧穿预算；等待期间催办通知 ~10 次/秒（mock 出站，无害）。
// 业务语义（催办→继续等待→升级→二次超时报错）与生产完全一致，仅时间尺度不同；
// 精确时长语义由 internal/workflows TestSuite（虚拟时间控制）覆盖。
package integration

import (
	"time"

	"b2b_pilot_v3/internal/domain"
)

// DemoDuration 本地演示/测试模式的虚拟时长（秒级；见本文件头部说明）。
const DemoDuration = 10 * time.Second

// NewDemoPolicy 本地演示/测试模式策略：全部 Timer 时长 = DemoDuration。
func NewDemoPolicy() BusinessConfigProvider { return demoPolicy{} }

type demoPolicy struct{}

func (demoPolicy) RemindAfter() time.Duration           { return DemoDuration }
func (demoPolicy) EscalateAfter() time.Duration         { return DemoDuration }
func (demoPolicy) ReceiptExceptionAfter() time.Duration { return DemoDuration }
func (demoPolicy) PreOrderEscalateAfter() time.Duration { return DemoDuration }
func (demoPolicy) AutoApprove(stageCode, grade string, amountCents int64) bool {
	return false // 基线全人工闸门（与 Static 一致）
}
func (demoPolicy) Classify(m domain.Modification) domain.ModificationClass {
	return domain.ClassifyModification(m)
}
func (demoPolicy) TargetState(class domain.ModificationClass) (string, error) {
	return domain.ModifyTargetState(class)
}
