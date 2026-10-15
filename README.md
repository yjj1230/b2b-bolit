# B2B 订单履约系统（b2b_pilot_v3）

长安仁恒订单履约验证项目：Go + PostgreSQL + SpiceDB（RBAC/ReBAC）+ Temporal + 企业微信。
订单履约闭环（ORD 订单创建 → STK 备货审批 → SHP 发货出库 → INV 开票回单），以「核心图谱」为起点向外派生文档，用 code-review-graph 做 roundtrip 验收。

## 文档体系

- **`docs/`** —— 既有验证项目文档家族（01 核心图谱 / 02 架构 / 03 测试 / 04 开发计划 / 07 企微接入 / 07a 交互 PRD / 08 外部接入 / 09 API Score Card）
- **`docs/v0.1/`** —— **0.1 发布版文档家族（最新）**：11 个章节（双 graph / 产品 / 交互 / 架构 / 业务流程 / 企微对接 / 数据库 / API / 测试 / 部署 / 迁移），以 `ord_fulfill` demo 为现实需求裁决源重构
- **`docs/gap-analysis.md`** —— 0.1 重构的差距分析与重构决策记录

## 0.1 版（08-21 上线）与 demo 的关系

- 裁决原则：**`ord_fulfill` demo（已集成运营/产品/客户直接反馈）为 0.1 现实需求裁决源**；现有文档与之冲突的，以 demo 为准
- 0.1 范围：仅**虹励（商务）**使用；**不打扰现有飞书流程**（财务/仓管继续用飞书，原审批流不停，采用数据同步让飞书侧数据在本系统流里可见可操作）；自动下单移出一期（同城企微备案未完成）；锁库存暂不做
- 迭代方式：0.1 推上去用起来，使用中快速升级 0.2/0.3；**文档驱动迭代**——一个文档进去，所有东西自动跟它同步

## 数据库访问二分（D-006）

- **流程引擎数据**：`internal/repo` 直连 PostgreSQL（orders/order_items/order_events/stock/shipment/invoice/approval/preorder/evidence/outbox 等）。
- **订单数据实体**：`internal/orderdata` → `internal/pgapi` 经 HTTP 访问 products/customers/purchase-contracts；生产 Worker 需配置 `B2B_PGAPI_BASE_URL`、`B2B_PGAPI_API_KEY`、`B2B_PGAPI_TIMEOUT`。本地演示自动使用 `orderdata.NewDemoMemoryProvider()`。
- `migrations/007_pgapi_data_split.sql` 去除本地 customers/products 外键；本地两表降级 legacy，ID 映射存 `pgapi_entity_mappings`。
- 旧 ID 回填工具：`go run ./scripts/pgapi_backfill -key $B2B_PGAPI_API_KEY`（默认 dry-run；确认无未匹配/歧义后 `-apply`）。

## 快速开始

```bash
make up        # 起测试 PG + 迁移
make test-l0   # L0 单元测试
make test-tests # 黑盒套件（l0-l3）
go run ./scripts/fullchain  # 22 步 HTTP 全链 demo
```

部署与迁移详见 `docs/v0.1/10-deployment.html` 与 `docs/v0.1/11-migration.html`。

## Agent 下单 LLM 接入

Agent 下单中的 LLM 只负责从企微消息提取意图和候选字段，不能直接创建订单，也不负责权限、客户/产品/合同/价格匹配或审批。候选结果仍由后端确定性校验，并经过预订单人工确认后才进入正式订单审批流。

启用 LLM provider 时，将 API Key 通过 Secret Manager 或进程环境变量注入；不要写入仓库、配置文件或日志：

```text
B2B_AGENT_ENABLED=true
B2B_WECOM_INGRESS_ENABLED=true
B2B_AGENT_PARSER_PROVIDER=llm
B2B_AGENT_LLM_BASE_URL=http://121.40.34.96:18097/v1
B2B_AGENT_LLM_MODEL=gpt-5.6
B2B_AGENT_LLM_TIMEOUT=30
B2B_AGENT_LLM_API_KEY=<secret-manager-value>
```

`B2B_AGENT_PARSER_PROVIDER=mock` 保持本地规则解析。设置为 `llm` 但未提供 API Key 时服务会拒绝启动，避免静默降级。网关使用 OpenAI 兼容的 `POST /chat/completions` 接口，客户端固定 `temperature=0`，仅接受严格 JSON 数组；后端继续执行确定性校验、权限校验和审批闸门。
