# Agent 多轮下单与订单草稿归并设计 Spec

## 1. 文档状态与修订结论

- 状态：Spec v2，已吸收 2026-10-15 评审意见
- 日期：2026-10-15
- 关联 Spec：[Agent 下单企业级闭环设计](2026-10-14-agent-ordering-enterprise-design.md)
- 真相源：本系统的原始消息、订单会话、预订单草稿、正式订单、订单事件和工作流状态
- 本文只解决 Agent 多轮消息到“人工确认的预订单草稿”这一段，不改变正式订单 OrderWorkflow 的审批和履约职责

本次修订做出以下不可再解释的决定：

1. 新增 OrderConversation 作为多轮消息的归并载体；一个会话对应一个主预订单。
2. DraftVersion 保存合并后的累积完整快照，同时保存本轮增量补丁和差异。
3. 新增 PRE_COLLECTING，明确表示“正在等待用户补充信息”。
4. 草稿版本冲突是可恢复的业务拒绝，不得使 PreOrderWorkflow 失败退出。
5. 相似度只能生成候选会话，不能自动归并；无法确定时必须人工选择。
6. expires_at 是会话硬过期时间；EscalateAfter 只是过期前的提醒/升级 Timer，两者不承担同一语义。
7. 同一会话使用数据库行锁串行合并；锁定对象是 order_conversations，而不是由消息临时决定的预订单。
8. correlation_id 稳定锚定会话，消息 ID 只作为链路中的事件节点。

## 2. 背景与问题

用户可能把一笔订单拆成多轮消息：

~~~text
用户：我要 SKU-C68
用户：100 件
用户：送到上海
~~~

当前单消息解析模型容易产生“一条消息一个预订单”，或者让模型自行猜测多条消息的关系。本设计把问题限定为：

~~~text
原始消息去重
→ 选择订单会话
→ 提取本轮字段补丁
→ 后端确定性合并
→ 生成不可变草稿版本
→ 补齐后人工确认
→ 派生正式订单
~~~

LLM 只负责语言理解和候选字段提取。客户、产品、价格、权限、合同、库存、审批和正式订单状态均由后端确定性代码处理。

## 3. 目标与明确不做

### 3.1 必须达到

- 同一订单的多轮消息归并到一个 OrderConversation 和一个主 PreOrder。
- 同一群可以同时存在多个订单，不因 group_id 相同而错误合并。
- 每次成功合并都生成递增且不可变的 DraftVersion。
- 重复消息不重复调用模型、不重复生成草稿、不重复创建正式订单。
- 字段冲突、会话不明确、主数据不匹配和高风险结果进入人工处理。
- 确认卡片携带版本号，旧卡片只能得到可恢复拒绝，不能终止工作流。
- 已确认会话不会继续吸收新消息；新订单和已确认订单的后续变更有明确入口。
- 服务重启、模型超时、数据库重试和通知失败不会丢失原始消息或当前草稿。

### 3.2 明确不做

- 不让 LLM 自主决定会话归属并直接合并。
- 不让 LLM 直接创建正式订单或执行任何审批、权限和库存动作。
- 不把整个群聊永久当成一个订单上下文。
- 不在字段未完整或未经销售确认时创建正式订单。
- 不允许自然语言“直接下单”绕过销售确认和正式订单审批。
- 不在本 Spec 中替换 ORD/STK/SHP/INV 正式订单状态机。

## 4. 领域对象与数据边界

### 4.1 inbound_messages：消息事实表

一条平台消息只对应一条消息事实。它负责消息幂等和处理生命周期，不记录业务状态迁移。

~~~text
id
tenant_id
group_id
msg_id
sender_id
message_seq
reply_to_msg_id
raw_text_encrypted
text_hash
received_at
parse_status       // RECEIVED、PARSED、FAILED、IGNORED、SUSPENDED
retry_count
session_id         // 归并成功后回填，可为空
draft_version      // 归并成功后回填，可为空
correlation_id
last_error
created_at
updated_at
~~~

唯一键： (tenant_id, group_id, msg_id)。

inbound_messages 保存原文、解析和入站处理状态；pre_order_events 只保存预订单状态迁移、人工动作和业务审计事件。后者通过 message_id 或 correlation_id 引用前者，但不重复保存原始文本。

### 4.2 order_conversations：会话归并事实

一个会话表示用户正在补充的一笔订单，不等于一个群聊。

~~~text
session_id
tenant_id
group_id
customer_id
owner_sales_id
pre_order_id
current_draft_version
last_draft_version
confirmed_draft_version
status              // OPEN、WAITING_CONFIRM、ESCALATED、CONFIRMED、EXPIRED、MANUAL_REQUIRED
last_message_at
escalate_at
expires_at
correlation_id
created_at
updated_at
~~~

关系约束：

~~~text
一个 OrderConversation → 一个主 PreOrder
一个 PreOrder → 多个不可变 DraftVersion
一个 OrderConversation → 多条 inbound_messages
~~~

新消息不再通过“新建一个预订单”表达多轮补充，而是先落到 inbound_messages，再由会话归并器决定是否更新现有会话。

会话与主预订单在同一个数据库事务中创建：先生成 session_id 和 correlation_id，再插入 order_conversations 与 pre_orders，并建立租户内外键；事务提交前不得把消息标记为已归并。任何一步失败都整体回滚，禁止产生没有主预订单的会话或没有会话归属的已处理消息。

### 4.3 pre_order_drafts：不可变草稿版本

每个版本同时保存累积快照和本轮变化：

~~~text
pre_order_id
draft_version
snapshot_json       // 合并后的累积规范化草稿，确认和派单读取此字段
patch_json          // 本轮 LLM/人工提交的增量字段
diff_json           // 相对上一版本的字段差异
source_msg_id       // 根消息；多条来源由 pre_order_draft_sources 关联表保存
evidence_json
policy_result_json
model_name
model_version
prompt_version
policy_version
status              // CURRENT、SUPERSEDED、CONFIRMED、REJECTED、CONFLICT
created_by
created_at
~~~

唯一键： (tenant_id, pre_order_id, draft_version)。

同一 pre_order_id 只能有一个 CURRENT 版本。CONFLICT 版本仍占用单调递增的 draft_version 序列，但不参与 CURRENT 的部分唯一索引，也不会改变 current_draft_version。历史版本永不覆盖，确认和派单只允许读取通过策略校验的 CURRENT 版本；确认事务先读取并校验 CURRENT 版本，再将它标记为 CONFIRMED，同时写入 confirmed_draft_version。确认完成后不再读取 CURRENT 版本，而通过 confirmed_draft_version 读取已确认版本。

## 5. 会话归并规则

会话选择按以下顺序执行，命中一条即停止：

1. 用户点击确认卡片上的“继续补充”；
2. 用户回复或引用某条订单消息；
3. 消息明确包含订单号、预订单号或草稿号；
4. 当前用户在该群只有一个未过期、未确认会话；
5. 相似度模块仅生成最多三个候选会话，展示给用户选择。

第 5 条不执行自动归并，也不以相似度超过阈值直接决定订单归属。用户没有选择、候选数为零或候选仍有歧义时：

~~~text
OrderConversation.status = MANUAL_REQUIRED
PreOrder.state = PRE_AMBIGUOUS
不更新任何草稿版本
不创建正式订单
~~~

已确认会话收到新消息时采用固定规则：

- 结构化“新建订单”按钮或固定命令 /new-order 表示新订单，创建新的 OrderConversation；
- 结构化“修改订单”按钮、回复已确认订单消息，或明确携带订单号/预订单号，进入订单修改/售后入口，不重新打开预订单；
- 以上规则都未命中时，创建人工关联待办，不自动判断为新订单或修改订单，也不静默丢弃。

同一群的并发会话上限由租户配置，默认每个群最多 10 个 OPEN/WAITING_CONFIRM/ESCALATED 会话；超过上限必须人工处理。

## 6. LLM Parser 契约

LLM 每轮只输出本轮字段补丁，不输出“最终可下单结论”：

~~~json
{
  "parse_status": "OK",
  "changes": {
    "product_sku": "SKU-C68",
    "quantity": 100,
    "receive_address": "上海"
  },
  "field_confidence": {
    "product_sku": 0.96,
    "quantity": 0.99,
    "receive_address": 0.91
  },
  "evidence": [
    {"field": "quantity", "quote": "100 件", "message_id": "m-2"}
  ],
  "ambiguities": [],
  "model_name": "...",
  "model_version": "...",
  "prompt_version": "..."
}
~~~

Parser 输入可以包含：当前累积草稿、当前缺失字段、最近的会话消息和本轮消息。不能无限制传入整个群聊，也不能让 Parser 写数据库。

Parser 输出格式错误或模型超时：最多重试 3 次；仍失败则保留原始消息，状态设为 FAILED，生成人工待办。重试次数、超时和降级行为是固定配置，不使用“有限次数”等未量化表述。

## 7. 确定性合并器

合并器位于 internal/agentorder/merger.go，设计为无 IO 的纯函数：

~~~text
Merge(previousSnapshot, patch, source, policy) -> MergeResult
~~~

它不读取数据库、不调用模型、不发送通知。数据库事务、锁和版本号由上层服务负责。

合并顺序固定为：

1. 校验字段类型、单位、数量和金额范围；
2. 校验来源消息与会话权限；
3. 应用主数据匹配结果；
4. 按优先级写入字段：人工修改 > 用户明确补充 > 已确认草稿 > 模型推断；
5. 明确的“改成/更换/不要原来的”生成覆盖操作；
6. 没有明确修改语义的不同候选生成字段冲突，不静默覆盖；
7. 计算缺失字段、策略风险和是否可确认；
8. 输出完整累积快照、增量补丁和差异。

例如：

~~~text
v1: product=C68
v2: product=C68, quantity=100
v3: product=C68, quantity=100, address=上海
~~~

后续仅说“D90”而没有“改成 D90”等明确替换语义时，结果为 CONFLICT(product)：保留 C68 作为当前可确认快照，同时写入一个 status=CONFLICT 的不可确认版本记录本次冲突；last_draft_version 前进，current_draft_version 不前进，不能静默覆盖。

## 8. 状态机与生命周期

### 8.1 预订单状态

新增并明确以下状态和边：

~~~text
PRE_CAPTURED
    → PRE_COLLECTING

PRE_COLLECTING
    → PRE_COLLECTING       // 吸收新消息并生成新草稿版本
    → PRE_AMBIGUOUS        // 会话或字段冲突
    → PRE_PENDING_CONFIRM   // 必填字段完整且策略允许确认
    → PRE_ESCALATED        // 达到提醒/升级时间
    → PRE_EXPIRED          // 达到硬过期时间，终态
    → PRE_IGNORED          // 权限失败或人工忽略，终态

PRE_PENDING_CONFIRM
    → PRE_COLLECTING       // 用户补充消息，旧卡片失效
    → PRE_CONFIRMED        // 当前版本确认
    → PRE_MODIFIED         // 人工修改后确认
    → PRE_IGNORED
    → PRE_ESCALATED
    → PRE_EXPIRED          // 硬过期时间到达

PRE_AMBIGUOUS
    → PRE_COLLECTING       // 用户补充或选择会话
    → PRE_PENDING_CONFIRM
    → PRE_EXPIRED
    → PRE_IGNORED

PRE_ESCALATED
    → PRE_COLLECTING       // 收到有效补充
    → PRE_PENDING_CONFIRM
    → PRE_EXPIRED
    → PRE_IGNORED
~~~

### 8.2 超时语义

- escalate_at：从最后一次有效消息起计算，默认 24 小时；触发提醒和人工升级，状态进入 PRE_ESCALATED，但会话仍可继续。
- expires_at：从会话创建起计算，默认 72 小时；到期后进入 PRE_EXPIRED 和 OrderConversation.EXPIRED，不再自动吸收消息。
- 收到有效补充后重新计算 escalate_at，不延长 expires_at，避免会话无限续期。
- 过期后新消息按照“新订单 / 已确认订单引用 / 人工关联”规则处理。

## 9. 人工确认与旧卡片行为

确认卡片必须携带 pre_order_id 和 draft_version。确认时在工作流内执行版本校验：

~~~text
卡片版本 == 当前 CURRENT 版本
  → 允许继续确认

卡片版本 != 当前 CURRENT 版本
  → 写 STALE_CARD_REJECTED 事件
  → 发送“卡片已过期，请刷新”通知
  → Workflow 继续运行并回到选择器等待
~~~

版本冲突是业务结果，不是系统异常。ValidatePreOrderDraft 应返回结构化结果，例如 VALID、STALE、NOT_FOUND；只有数据库故障、网络故障等系统错误才允许让 Workflow 返回失败。

旧卡片拒绝后，新的确认卡片可以继续成功确认，工作流不能因为一次旧卡片点击而死亡。

## 10. 并发与事务边界

同一会话的合并使用 order_conversations 行锁串行化：

~~~text
BEGIN
  SELECT order_conversations
  WHERE session_id = ?
  FOR UPDATE
  读取 current_draft_version、last_draft_version
  校验消息未处理
  调用纯函数 Merger
  new_version = last_draft_version + 1
  INSERT pre_order_drafts(new_version)
  UPDATE order_conversations(last_draft_version, last_message_at, escalate_at)
  若结果可确认：同时 UPDATE current_draft_version
  UPDATE inbound_messages(session_id, draft_version, status)
COMMIT
~~~

冲突版本和正常版本都使用 new_version，因此版本号永远单调递增；只有可确认版本才推进 current_draft_version。有效补充后 escalate_at = now() + 24h，expires_at 保持原值不变。

同一会话不同时使用“数据库锁或乐观锁”两套未定义方案；本 Spec 采用数据库 FOR UPDATE 作为唯一并发机制，版本号用于审计和确认卡片防陈旧，不用于替代会话行锁。

不同会话可以并行处理。消息入站、草稿合并、通知发送和正式订单工作流启动之间使用 outbox 解耦。

## 11. correlation_id 与审计

会话创建时生成稳定的：

~~~text
correlation_id = agent:{tenant_id}:{session_id}
~~~

该值贯穿：

~~~text
inbound_messages
→ 会话选择
→ LLM 调用
→ DraftVersion
→ 策略校验
→ 人工确认
→ 正式订单
→ OrderWorkflow
~~~

单条消息的 msg_id、模型调用 ID 和审批任务 ID 作为链路节点记录，不能替代会话级 correlation_id。

## 12. 异常和恢复边界

- 重复消息：返回既有 inbound_messages 结果，不重新解析。
- 模型失败：最多重试 3 次，超限转人工，原文保留。
- 主数据或权限失败：不重试模型，记录确定性失败原因并转人工。
- 字段冲突：创建一个 status=CONFLICT 的不可确认版本，保留上一版本为 CURRENT，要求用户明确修改或转人工；不得静默覆盖。
- 通知失败：草稿事务不回滚，通知事件进入 outbox 重试。
- 数据库提交成功但工作流启动失败：订单和草稿不回滚，workflow outbox 退避重试，超过 8 次进入死信和人工补偿。
- 服务重启：从消息表、会话表、草稿 CURRENT 版本和 outbox 恢复，不依赖内存上下文。
- 旧卡片：可恢复拒绝，Workflow 继续等待。
- 会话过期：不自动重开，后续消息按新会话或人工关联处理。

## 13. 与当前代码的适配任务

1. 新增 migration 010：创建 inbound_messages 和 order_conversations；消息幂等由 inbound_messages 的 (tenant_id, group_id, msg_id) 唯一键承接。
2. migration 010 为 pre_orders 增加 conversation_id、root_msg_id 和 confirmed_draft_version；回填历史数据后建立 (tenant_id, conversation_id) 唯一约束，并删除 pre_orders 上旧的 (tenant_id, group_id, msg_id) 唯一约束。root_msg_id 只作审计字段，不再承担消息幂等。
3. migration 010 为 order_conversations 增加 current_draft_version、last_draft_version、confirmed_draft_version、escalate_at 和 expires_at；pre_order_id 建立租户内外键，创建会话和主预订单必须在同一事务中完成，禁止孤儿会话。
4. 扩展 pre_order_drafts：保留现有 source_msg_id、source_msg_hash、quote、policy_status、expires_at、customer_basis、product_basis、contract_basis、price_basis 字段；新增 snapshot_json、patch_json、created_by 和 status=CONFLICT。source_message_ids 不直接改成不透明多值字段，改用 pre_order_draft_sources 关联表保存一个版本吸收的多条消息。
5. pre_order_draft_sources 使用 (tenant_id, pre_order_id, draft_version, message_id) 唯一键，记录 message_id、source_role 和引用片段；source_msg_id 继续表示根消息，避免丢失现有迁移 009 的审计兼容性。
6. 将 IntentParser 契约从候选数组扩展为带版本、证据和字段补丁的结构化结果。
7. 新增纯函数合并器及表驱动单元测试。
8. 扩展 PreOrderFSM，加入 PRE_COLLECTING、PRE_EXPIRED 和完整入出边，并同步更新 domain.IsTerminal()。
9. 修改 PreOrderWorkflow，使新消息可生成版本并回到收集态；旧卡片冲突不能直接 return err 终止 Workflow。
10. 采用 order_conversations FOR UPDATE 串行化合并；任何版本都分配 last_draft_version + 1，只有可确认版本推进 current_draft_version。
11. 将 correlation_id 改为会话级稳定值。
12. 会话存档入口完成“消息落库 → 会话归并 → 草稿更新 → 启动 PreOrderWorkflow”的闭环，不能只推进存档游标。
13. 群会话达到上限时，inbound_messages.parse_status 设为 SUSPENDED，并记录 suspend_reason；恢复名额后才能重新处理，不允许留下无状态消息。

## 14. 验收测试矩阵

每条用例都必须有明确的 Given/When/Then：

| ID | Given / When | Then |
| --- | --- | --- |
| TC-01 | 同群无活跃会话，依次发送产品、数量、地址 | 只有 1 个会话、1 个主预订单，最终快照包含三项字段 |
| TC-02 | 同群两笔订单交错发送 | 产生 2 个会话，字段不交叉 |
| TC-03 | 重放相同 msg_id | 不新增版本、不再次调用模型 |
| TC-04 | v2 卡片期间产生 v3，再点击 v2 | 返回 STALE_CARD_REJECTED，Workflow 继续存活，v3 可确认 |
| TC-05 | 已有 C68，仅发送“D90” | 保留 C68，生成 draft_version=4 且 status=CONFLICT，current_draft_version 仍为 3；不静默覆盖 |
| TC-06 | 用户明确数量 100，模型推断数量 50 | 保留 100，记录模型候选被拒绝 |
| TC-07 | 销售人工改数量 200，用户发送 100 | 保留 200，人工修改优先 |
| TC-08 | 无客户权限的发送者发完整订单 | PRE_IGNORED，无正式订单，写审计 |
| TC-09 | PRE_PENDING_CONFIRM 收到新地址 | 生成新版本，回 PRE_COLLECTING，旧卡片失效 |
| TC-10 | 缺少必填收货地址时点击确认 | 不派生正式订单，返回缺失字段 |
| TC-11 | 用户说“直接下单”但未销售确认 | 拒绝绕过，不创建正式订单 |
| TC-12 | 模型连续失败 | 第 3 次后转人工，原始消息仍可查询 |
| TC-13 | 已确认会话收到新消息 | 仅结构化“新建订单”按钮或 /new-order 创建新会话；仅结构化“修改订单”按钮、reply_to_msg_id 或明确订单号进入修改入口；其余一律 MANUAL_REQUIRED |
| TC-14 | 两个会话都可能匹配，消息只写“改 100” | 只展示候选，不自动归并，进入人工选择 |
| TC-15 | 同会话两条消息并发合并 | FOR UPDATE 串行执行，版本严格递增，无覆盖 |
| TC-16 | 达到 expires_at 后收到消息 | 原会话进入 EXPIRED，不吸收；按新订单/引用/人工规则处理 |
| TC-17 | 订单事务提交后 Temporal 启动失败 | outbox 退避重试，最终启动成功或进入死信，不回滚订单 |
| TC-18 | 按会话 correlation_id 查询 | 能串起消息、会话、LLM、草稿、策略、确认、正式订单和 Workflow |
| TC-19 | 当前版本为 3，下一条消息形成冲突 | 冲突版本占用 4，current_draft_version 仍为 3；下一条合法补充占用 5，不发生主键冲突 |
| TC-20 | 有效补充消息到达 | last_draft_version 和 draft_version 正确递增，escalate_at 顺延 24 小时，expires_at 保持不变 |

Spec 只有在以上 20 条均能由固定输入得到固定结果后，才允许进入实现计划阶段。

## 15. 上线门禁

- 消息级幂等、会话级归并和订单级幂等分别可验证。
- 未确认的草稿不会产生 ORD_REQUESTED。
- 旧卡片不会终止 Workflow，也不能确认新版本。
- 不明确的会话归属和字段冲突不会自动合并。
- 过期会话不会无限续期或静默吸收新消息。
- 任意正式订单都可由 correlation_id 追溯到全部原始消息和草稿版本。
- 模型不可用、主数据失败、通知失败和 Temporal 启动失败都有可观察、可重试或可人工接管的结果。
- 多租户、权限、敏感消息和审计字段通过安全评审。

## 16. 结论

本 Spec 将多轮下单严格拆成四个边界：

~~~text
消息事实边界：inbound_messages
会话归并边界：order_conversations
草稿裁决边界：DraftVersion + 确定性合并器
正式订单边界：人工确认后进入 OrderWorkflow
~~~

LLM 只在第二和第三个边界之间提供语言候选；它不能跨越草稿确认边界直接触碰正式订单。所有状态、权限、主数据、版本、并发和审批结果都由后端确定性代码和人工闸门裁决。

