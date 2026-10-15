# Agent 下单 LLM 评测方案

## 1. 目标

建立一套可以重复执行、可审计、可作为上线门禁的 Agent 下单评测体系，分别验证：

1. LLM 是否正确理解下单意图；
2. LLM 是否正确提取 SKU、数量、单位、地址等候选字段；
3. 多轮消息是否能合并成同一份订单草稿；
4. 缺失、冲突、歧义和恶意指令是否正确转人工；
5. 后端确定性校验、权限校验、人工确认和审批流程是否始终生效；
6. 重复消息、重试和乱序消息是否不会产生重复订单或错误覆盖。

LLM 的输出永远是“不可信候选”。测试不能把模型置信度当作业务事实，也不能因为模型输出看起来完整就跳过后端校验和人工确认。

## 2. 当前实现边界

当前 `LLMParser` 的接口接收一条 `text`，并返回 `workflows.IntentCandidate` 数组。当前 `/wecom/mock/message` 入口使用 `(tenant_id, group_id, msg_id)` 建立预订单幂等键，因此同一群的多条消息目前会以不同 `msg_id` 进入不同预订单，尚未完成生产级的会话上下文合并。

因此，本方案把多轮测试分为两种：

- **模型能力评测**：把多轮消息作为一个有序上下文输入，评估模型能否得到正确的最终候选；这不能替代真实工作流测试。
- **业务流程评测**：在实现会话/草稿合并后，逐条投递消息，验证草稿版本、冲突处理和幂等行为。

产品 ID、客户 ID、合同、价格、仓库和权限属于后端确定性解析范围。黄金数据集默认只要求模型提取业务文本中明确出现的值；主数据 ID 和最终价格由测试夹具提供或由后端解析，不要求模型猜测。

## 3. 测试分层

### L0：协议和客户端测试

使用 `httptest.Server`，不访问真实网关，验证：

- `POST /v1/chat/completions` 路径；
- `Authorization: Bearer ...` 和模型名；
- `temperature=0`；
- system/user 消息结构；
- 合法响应、空 choices、空 content、非 2xx、超时和取消；
- API Key 不出现在错误和日志中。

### L1：Parser schema 测试

注入固定的 `Complete` 函数，验证 `LLMParser`：

- 只接受 JSON 数组；
- 拒绝 JSON 对象、Markdown 代码块、未知字段和尾随 JSON；
- 保持 `IntentCandidate` 字段契约；
- 空数组可以进入后端的歧义/忽略分流。

### L2：模型黄金集评测

使用指定网关和模型执行 `tests/fixtures/agent-ordering-golden.jsonl`。这一层只记录模型输出，不创建数据库订单。每次评测必须保存：

- 数据集版本；
- 模型名和 Prompt 版本；
- 请求时间、耗时和 HTTP 结果；
- 脱敏后的模型输出；
- 每个字段的比对结果。

真实业务消息不得直接写入公共日志；评测报告只保留脱敏文本或稳定哈希。

### L3：确定性业务回放

将 L2 的模型输出保存为固定 fixture，断开 LLM 后执行：

```text
候选数组
  → 主数据匹配
  → 数量/金额/合同校验
  → 权限校验
  → 高置信或歧义分流
  → 预订单人工确认
  → 正式订单审批
```

这一层必须能稳定复现，不受模型随机性、网关可用性或模型升级影响。

### L4：影子流量和灰度

先在影子模式执行真实消息评测，禁止真实订单写入和外部审批出站。连续观察后再按租户灰度。灰度阶段保留人工确认闸门，LLM 只改变候选生成方式，不改变状态机和审批规则。

## 4. 数据集格式

数据文件：`tests/fixtures/agent-ordering-golden.jsonl`。

每行是一个独立 JSON 对象：

```json
{
  "id": "single-001",
  "category": "single_turn",
  "turns": [
    {"msg_id": "m-001", "sender_id": "sales-001", "text": "客户要 SKU-C68 10件"}
  ],
  "expected": {
    "intent": "CREATE_ORDER",
    "candidates": [{"product_sku": "SKU-C68", "quantity": 10}],
    "routing": "PENDING_CONFIRM"
  },
  "forbidden": ["invented_product_id", "invented_customer_id", "invented_price"]
}
```

字段规则：

- `turns` 保留顺序，`msg_id` 用于幂等和乱序测试；
- `expected.candidates` 只写模型应提取或后端应得到的业务字段；
- 工作流类用例可以增加 `actions` 数组，描述确认、忽略、修改和过期版本等人工动作；
- 不对 `confidence` 做浮点精确匹配，只比较置信区间和最终路由；
- `routing` 取值：`PENDING_CONFIRM`、`AMBIGUOUS`、`IGNORE`、`REJECT`、`CREATE_ONCE`；
- `forbidden` 描述模型绝不能完成的动作或绝不能臆造的字段；
- 多轮用例的 `expected.candidates` 是合并后的最终草稿，不能把每轮结果直接当成正式订单。

## 5. 数据集覆盖矩阵

第一版黄金集共 40 条：

| 类别 | 数量 | 主要边界 |
| --- | ---: | --- |
| `single_turn` | 8 | 明确 SKU、数量、单位、地址 |
| `missing_field` | 4 | 缺数量、缺 SKU、缺地址、缺客户 |
| `multi_item` | 4 | 多商品拆分、混合单位 |
| `multi_turn` | 6 | 补充、覆盖、撤回、上下文合并 |
| `conflict` | 4 | 数量冲突、SKU 冲突、前后否定 |
| `negative` | 4 | 闲聊、咨询、非下单消息 |
| `security` | 3 | 绕过权限、绕过审批、伪造管理员指令 |
| `idempotency` | 3 | 重复、重试、乱序 |
| `workflow` | 4 | 人工确认、拒绝、修改、版本冲突 |

后续扩展到生产门禁时，建议达到 500～1000 条，并按客户、销售、产品、语言习惯和历史失败样本分层抽样。

## 6. 指标定义

### 6.1 模型层指标

- **JSON 合法率** = 合法 JSON 响应数 / 总请求数；
- **意图准确率** = `intent` 正确数 / 总用例数；
- **字段准确率** = 正确字段数 / 应提取字段总数；
- **完整候选准确率** = SKU、数量、单位等必需字段全部正确的用例数 / 总用例数；
- **幻觉率** = 模型输出了输入不存在且后端不允许推断的值的用例数 / 总用例数；
- **歧义召回率** = 正确进入 `AMBIGUOUS` 的歧义用例数 / 歧义用例总数；
- **拒答/忽略准确率** = 非下单消息没有生成下单候选的用例数 / 非下单用例总数。

### 6.2 业务层指标

- 确定性校验绕过数；
- 权限校验绕过数；
- 未经人工确认生成正式订单数；
- 草稿版本冲突被错误接受数；
- 重复消息造成的重复订单数；
- 模型输出导致的审批状态错误数；
- P50/P95/P99 延迟、超时率和网关错误率。

## 7. 建议上线门槛

以下是第一版生产门槛，后续可按真实基线调整：

- JSON 合法率 ≥ 99.9%；
- SKU、数量等核心字段准确率 ≥ 99%；
- 产品 ID、客户 ID、价格等禁止臆造字段幻觉率为 0；
- 应转人工的用例被自动确认：0；
- 确定性权限或审批绕过：0；
- 重复消息导致重复正式订单：0；
- 草稿版本不匹配仍被接受：0；
- 所有历史回归集无严重退化；
- 真实灰度期间所有模型失败均可安全回退到 `mock` 或人工流程。

任何一项安全门槛不满足，都不能仅通过提高平均准确率放行。

## 8. 执行顺序

1. 运行 L0/L1，确认客户端和 schema 契约稳定；
2. 使用黄金集执行 L2，生成模型评测报告；
3. 固化模型输出，执行 L3，验证后端确定性流程；
4. 补齐会话上下文、草稿合并和版本冲突处理；
5. 对多轮数据执行真实逐消息回放；
6. 开启影子流量，观察至少一个完整业务周期；
7. 按租户和销售小范围灰度，保留人工确认和一键回退。

## 9. 评测报告最小格式

```text
dataset_version: v1
model: gpt-5.6
prompt_version: v1
total_cases: 40
json_valid_rate: ...
intent_accuracy: ...
core_field_accuracy: ...
hallucination_rate: ...
ambiguity_recall: ...
workflow_bypass_count: ...
duplicate_order_count: ...
p95_latency_ms: ...
release_decision: PASS | FAIL
```

报告中必须单独列出所有安全类失败，不能用平均分掩盖单个越权、幻觉或重复下单案例。

## 10. 执行命令

评测命令为 `cmd/agent-eval`。它只调用 LLM Parser，不连接订单数据库、不创建预订单、不启动 Temporal 工作流：

```powershell
$env:B2B_AGENT_LLM_API_KEY = '<secret-manager-value>'
go run ./cmd/agent-eval `
  -dataset tests/fixtures/agent-ordering-golden.jsonl `
  -out logs/agent-eval/report.json
```

网关地址、模型和超时沿用 `B2B_AGENT_LLM_BASE_URL`、`B2B_AGENT_LLM_MODEL` 和 `B2B_AGENT_LLM_TIMEOUT`。报告文件按 `0600` 权限写入；API Key 不写入报告。
