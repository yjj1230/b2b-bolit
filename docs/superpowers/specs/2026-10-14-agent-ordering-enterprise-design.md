# Agent 下单企业级闭环设计

## 状态与范围

- 状态：已获用户确认的设计基线
- 日期：2026-10-14
- 真相源：本系统 `orders`、`order_items`、订单事件和 `OrderWorkflow`
- 迁移策略：旧飞书桥接保留为人工兜底和迁移通道，Agent 正式订单不再以旧飞书为主执行系统

## 背景

当前仓库已经有预订单状态机、候选解析接口、销售确认 Signal 和正式订单派生逻辑，但企业级闭环尚未完成：

1. 意图识别和消息自动入口只在 `deferred` 构建中存在；默认构建没有真实消息捕捉路径。
2. `LLMParser` 尚未接线，当前只有 MockParser。
3. `deriveOrder` 创建正式订单后没有可靠启动 `OrderWorkflow`，可能留下 `ORD_REQUESTED` 孤儿订单。
4. Agent 派生订单仍使用默认客户、渠道、账期和仓库等演示值。
5. 预订单候选缺少版本化证据、规则命中结果和完整审计上下文。

本设计将 Agent 定义为“生成可审阅订单草稿的候选系统”。Agent 不拥有签约、报价、审批或正式订单创建权限；确定性业务规则和人工确认共同决定是否产生正式订单。

## 目标

### 必须达到

- 真实企微消息可以安全、可恢复地进入系统。
- 同一消息重复拉取、重复回调和重复确认不会产生重复正式订单。
- 没有销售确认就不会生成正式订单。
- 销售确认后，正式订单最终一定会启动本系统 `OrderWorkflow`，或进入可见的补偿队列。
- 每个正式订单都能追溯到原始消息、草稿版本、模型版本、规则版本和确认人。
- Agent 解析失败、主数据无法匹配或风险规则命中时，可靠降级到人工下单。
- 旧飞书桥接可以在迁移期继续承接人工兜底，但不改变本系统作为 Agent 订单真相源的定位。

### 不在本次设计内

- 让 Agent 自动绕过销售确认。
- 让 Agent 直接执行价格审批、董事长审批、备货、发货或开票。
- 一次性替换所有旧飞书人工审批流程。
- 用模型判断库存、合同或权限；这些判断必须由主数据和确定性策略完成。

## 目标架构

```text
企微会话存档/消息回调
        |
        v
消息归一化 + 验签解密 + 消息幂等
        |
        v
Agent Parser -----> 候选/证据结构化结果
        |
        v
确定性业务策略
客户、SKU、单位、价格、合同、账期、额度、交期、风险
        |
        +---- 失败/高风险 ----> 人工待办或人工下单兜底
        |
        v
版本化订单草稿 PRE_* 
        |
        v
归属销售确认/修改/忽略
        |
        v
正式订单 + OrderRequested 事件 + workflow_start outbox
        |
        v
Outbox Dispatcher 幂等启动 OrderWorkflow
        |
        v
审价 → 终审 → 合同 → 备货 → 发货 → 开票 → 回款 → 关闭
```

## 核心领域边界

### 1. 消息接入层

负责真实企微通道，不负责业务裁决：

- 会话存档或官方消息通道接入；
- 验签、解密、租户归一化和群上下文绑定；
- 游标 checkpoint、断点续传和重试；
- 原始消息哈希与敏感字段脱敏；
- 以 `(tenant_id, group_id, msg_id)` 做消息幂等；
- 入站失败进入可重试队列和死信队列。

`POST /wecom/mock/message` 只保留为测试注入入口，不能作为生产消息源。

### 2. Agent Parser

Parser 只生成结构化候选，不直接写订单：

```go
type IntentParseResult struct {
    Candidates  []IntentCandidate
    Evidence    []IntentEvidence
    ModelName   string
    ModelVersion string
    ParseStatus string
}
```

每个候选至少包含：客户、产品、数量、单位、单价、账期、交期、收货地址、原始引用片段和置信度。

Parser 必须满足：

- 固定 JSON Schema；
- 结构化输出失败可重试；
- 模型版本、提示版本和输入哈希可记录；
- 低置信、多候选和字段缺失不自动派生；
- LLM 不负责主数据最终匹配；
- Parser 不拥有数据库写权限。

### 3. 确定性业务策略层

将模型候选转换为 `VALID`、`NEEDS_CONFIRMATION`、`REJECTED` 或 `MANUAL_REQUIRED`。策略至少覆盖：

- 群与客户的归属绑定；
- 消息发送者与客户的权限；
- SKU 唯一匹配和单位换算；
- 客户等级、协议价、价格偏差；
- 合同和账期匹配；
- 信用额度和金额阈值；
- 收货地址、交期和仓库候选；
- 新客户、高金额、异常价格、多候选和缺字段升级规则。

策略结果必须保存命中的规则 ID、输入快照、输出结果和规则版本，不能只保存一个布尔值。

### 4. 版本化订单草稿

扩展预订单模型，保留不可变草稿版本：

- `pre_order_id`；
- `draft_version`；
- 原始消息 ID、消息哈希和引用片段；
- Agent 候选；
- 确定性校验结果；
- 客户/产品/合同/价格的匹配依据；
- 模型、提示和策略版本；
- 草稿状态和过期时间；
- 修改前后差异。

销售修改时创建新版本，不覆盖旧版本。确认必须携带版本号，版本不匹配时拒绝并要求刷新卡片。

### 5. 人工确认与权限

确认动作仍由 `preorder-confirm`、`preorder-ignore` 等受控 Signal 进入工作流：

- 只允许归属销售或管理员确认；
- API 层和工作流层都做权限校验；
- `operator` 以服务端鉴权身份为准；
- 确认成功后写入确认人、确认时间和草稿版本；
- 同一草稿只能确认一次；重复请求返回幂等结果；
- 低置信、高金额、新客户或异常价格可以强制二次人工审批。

### 6. 正式订单派生和工作流启动

确认后的正式订单创建必须与事件和 workflow outbox 在一个数据库事务内完成：

```text
BEGIN
  INSERT orders
  INSERT order_items
  INSERT OrderRequested event
  INSERT workflow_start workflow_outbox
COMMIT
```

Dispatcher 读取 workflow outbox 后，以稳定的 `WorkflowID(tenant, idempotency_key)` 启动 `OrderWorkflow`。启动成功标记事件完成；失败按退避策略重试，超过阈值进入死信和人工补偿。

禁止在数据库事务内部直接调用 Temporal，避免数据库和工作流出现不可恢复的半成功状态。

派生订单必须从经过确认的草稿取得客户、产品、数量、价格、账期、渠道和仓库候选，不允许再使用 `cust-0001`、`款到`、`渠道A` 或 `wh-yangyuan` 等演示默认值。

## 数据和接口变化

### 需要扩展的持久化对象

- `pre_orders`：当前草稿版本、来源消息、解析状态、过期时间；
- `pre_order_drafts`：版本化候选和证据；
- `pre_order_decisions`：确认、修改、忽略和升级动作；
- `policy_evaluations`：规则输入、规则版本、命中结果；
- `workflow_outbox_events`：工作流启动和订单域通知派发；现有 `outbox_events` 继续只服务 SpiceDB 权限投影，避免不同消费者共享未知事件类型；
- `dead_letter_events`：无法自动恢复的入站、派生和启动失败。

### 需要保持的幂等键

```text
消息幂等：tenant_id + group_id + msg_id
草稿幂等：pre_order_id + draft_version
订单幂等：tenant_id + derived_order_key
工作流幂等：tenant_id + order_idempotency_key
```

### 建议增加的接口

- `POST /api/v1/preorders/{id}/drafts`：提交或更新候选草稿；
- `GET /api/v1/preorders/{id}/evidence`：查看原始消息和解析证据；
- `POST /api/v1/preorders/{id}/confirm`：带版本号的确认命令；
- `GET /api/v1/agent/orders/{id}/trace`：查看消息到订单的完整链路；
- 内部 outbox 重试、补偿和死信管理接口。

现有 Signal 端点可以继续兼容企微卡片回调，但新确认接口要复用相同的权限、幂等和审计逻辑。

## 可靠性和安全要求

- 所有租户、群、客户和订单查询带租户边界；
- 原始消息、客户联系方式和合同数据按敏感等级加密或脱敏；
- 模型密钥和企微会话存档密钥进入 Secret/KMS 管理；
- Parser、策略层、确认层和工作流之间使用 correlation ID；
- 入站、解析、策略、确认、派生、启动和补偿都有结构化日志；
- 服务重启后可以从 checkpoint、outbox 和死信恢复；
- 对 `ORD_REQUESTED` 没有对应 workflow 的订单设置告警和自动巡检；
- 所有自动化动作均支持人工暂停和人工接管。

## 测试和发布门禁

### 测试层级

1. Parser Schema、单位换算和证据提取单测；
2. 业务策略表驱动测试；
3. 消息重复、乱序、断点和解密失败测试；
4. 确认重复点击、版本冲突和权限负向测试；
5. 数据库提交成功但 workflow 启动失败的 outbox 故障注入测试；
6. 真实企微回调、会话存档和订单主链 E2E；
7. 灰度期间 Agent 草稿与人工订单的影子对账。

### 上线门禁

- 重放同一消息不会产生重复正式订单；
- 未确认的预订单不会生成正式订单；
- 确认后的正式订单最终会启动 `OrderWorkflow` 或进入可见补偿队列；
- 每个订单可追溯到消息、草稿版本、模型版本、策略版本和确认人；
- Parser 失败和主数据缺失可以稳定降级到人工；
- 不存在长期未处理的 `ORD_REQUESTED` 孤儿订单；
- 真实环境权限、回滚、重试、死信恢复和租户隔离测试通过。

## 分阶段实施

### Phase 1：先闭合内部订单链

- 修复 `deriveOrder` 后的工作流启动；
- 增加 outbox、Dispatcher、重试和死信；
- 增加孤儿订单巡检；
- 去掉 Agent 派生订单的演示硬编码；
- 补齐确认幂等和版本校验。

### Phase 2：补齐草稿和确定性策略

- 增加草稿版本、证据、策略评估和决策审计表；
- 接客户、产品、合同、价格和额度主数据；
- 建立风险分级和人工升级规则；
- 保留旧人工桥接作为降级路径。

### Phase 3：接入真实 Agent 和企微通道

- 接真实模型供应商；
- 接会话存档、解密、游标和重试；
- 绑定真实群客户和归属销售；
- 完成真实企业微信 E2E 验收。

### Phase 4：影子运行与灰度

- Agent 先只生成草稿和确认卡；
- 与人工订单做字段、金额和客户归属对账；
- 达到准确率、重复率、审计完整性和工作流启动门禁后扩大范围；
- 最后再评估是否减少人工确认范围。

## 现有源码映射

- `internal/wecom/intent.go`：生产 Parser 适配和版本记录；
- `cmd/wecom/deferred.go`：生产消息接入、checkpoint 和重试；
- `internal/workflows/preorder.go`：草稿版本、确认幂等、派生命令和工作流启动触发；
- `internal/workflows/activities.go`：确定性业务校验和策略执行；
- `internal/repo/preorder.go`：预订单、草稿、决策和证据持久化；
- `internal/repo/orders.go`：订单事件和 outbox 事务边界；
- `internal/api/server.go`：确认接口、补偿接口和查询投影；
- `internal/workflows/client.go` / `internal/api/localrunner.go`：生产与本地工作流启动实现；
- `migrations/`：新增草稿、策略、outbox 和死信表；
- `tests/l2_e2e/`、`internal/workflows/`：补全 E2E、故障注入和重放测试。

## 关键决策

1. Agent 是候选生成器，不是订单裁决者。
2. 本系统订单表和 `OrderWorkflow` 是 Agent 订单的唯一真相源。
3. 正式订单创建和工作流启动通过 Outbox 解耦并保证最终一致性。
4. 旧飞书桥接在迁移期作为人工兜底，不作为 Agent 订单主路径。
5. 先影子运行，再灰度放量；任何自动化能力都必须可审计、可重放、可人工接管。
