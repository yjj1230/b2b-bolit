# Agent Ordering Enterprise Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 Agent 下单从“解析候选”完善为可审计、可恢复、幂等并经过人工确认的企业级订单闭环，最终由本系统的 `orders`、订单事件和 `OrderWorkflow` 作为唯一真相源。

**Architecture:** Agent 只产生带证据的版本化预订单草稿；确定性策略层负责客户、SKU、价格、合同、额度、权限和风险裁决；销售确认后，订单、`OrderRequested` 事件和独立的 workflow outbox 在同一事务写入。Dispatcher 从 workflow outbox 幂等启动 `OrderWorkflow`，失败重试并进入死信；企微消息入口、解析失败和策略失败均可恢复并降级人工。

**Tech Stack:** Go, PostgreSQL/pgx, Temporal Go SDK, SpiceDB projector, 企业微信 Gateway, HTTP API, Go test。

**Spec:** `docs/superpowers/specs/2026-10-14-agent-ordering-enterprise-design.md`

## Global Constraints

- Agent 是候选生成器，不拥有签约、报价、审批、备货、发货或开票权限。
- 本系统 `orders`、`order_items`、订单事件和 `OrderWorkflow` 是 Agent 订单唯一真相源。
- `workflow_outbox_events` 独立于现有 SpiceDB `outbox_events`；不得混用事件消费者。
- 消息幂等键为 `tenant_id + group_id + msg_id`；草稿幂等键为 `pre_order_id + draft_version`；订单幂等键为 `tenant_id + derived_order_key`。
- 未经过销售确认的预订单不得创建正式订单；确认请求必须带 `draft_version`。
- 派生订单不得使用 `cust-0001`、`款到`、`渠道A` 或 `wh-yangyuan` 等演示默认值。
- 所有查询和写入必须带租户边界；敏感消息和合同信息按现有安全配置脱敏或加密。
- 任何自动化动作都必须记录 correlation ID，并支持重试、死信、人工暂停和人工补偿。
- 每个任务先写失败测试，再实现最小代码，最后运行对应测试和全量检查；不能以未运行测试的状态宣称完成。

---

### Task 1: 持久化模型、事务接口和迁移

**Files:**
- Create: `migrations/009_agent_ordering_enterprise.sql`
- Create: `internal/repo/preorder_drafts.go`
- Create: `internal/repo/workflow_outbox.go`
- Modify: `internal/repo/models.go`
- Modify: `internal/workflows/store.go`
- Test: `internal/repo/preorder_drafts_test.go`, `internal/repo/workflow_outbox_test.go`

**Interfaces:**
- Produces `repo.PreOrderDraft`, `repo.PreOrderDecision`, `repo.PolicyEvaluation`, `repo.WorkflowOutboxEvent`, `repo.DeadLetterEvent` and repository methods used by later tasks.
- `Store` gains `GetCurrentPreOrderDraft`, `CreatePreOrderDraftVersion`, `RecordPreOrderDecision`, `RecordPolicyEvaluation`, `EnqueueWorkflowOutbox`, `ClaimWorkflowOutbox`, `MarkWorkflowOutboxDone`, `RetryWorkflowOutbox`, `DeadLetterWorkflowOutbox`.

- [ ] **Step 1: Write the failing migration/model tests.** Assert draft versions are immutable, `(pre_order_id, draft_version)` is unique, one current draft is selected, workflow outbox claims are lease based, and dead-letter rows preserve the original payload and error.
- [ ] **Step 2: Run the focused tests to verify they fail.** Run `go test ./internal/repo -run 'PreOrderDraft|WorkflowOutbox' -count=1`; expected failure because the tables and methods do not exist.
- [ ] **Step 3: Add the migration.** Create tenant-scoped tables `pre_order_drafts`, `pre_order_decisions`, `policy_evaluations`, `workflow_outbox_events`, and `dead_letter_events` with JSONB evidence/input/output, model/prompt/policy versions, status, retry counters, lease fields, timestamps, unique constraints, and indexes for pending claims and `ORD_REQUESTED` reconciliation.
- [ ] **Step 4: Add typed models and repository methods.** Use pgx transactions and `FOR UPDATE SKIP LOCKED` for outbox claims; never overwrite a draft version; return an idempotent existing row when the same event key is inserted twice.
- [ ] **Step 5: Extend `workflows.Store` and update `repo.Repo` compile implementations.** Keep all side effects behind the Store interface so fake stores used by workflow tests remain deterministic.
- [ ] **Step 6: Run the focused tests and migration lint.** Run `go test ./internal/repo -run 'PreOrderDraft|WorkflowOutbox' -count=1` and verify migration SQL has tenant predicates and no reuse of `outbox_events`.

### Task 2: Versioned draft lifecycle, confirmation idempotency, and audit evidence

**Files:**
- Modify: `internal/workflows/preorder.go`
- Modify: `internal/api/signals.go`
- Modify: `internal/repo/preorder.go`
- Modify: `internal/repo/models.go`
- Test: `internal/workflows/preorder_test.go`, `internal/api/signals_test.go`, `internal/repo/preorder_test.go`

**Interfaces:**
- `PreOrderConfirmSignal` adds `DraftVersion int` and keeps server-side operator stamping.
- `PreOrderWorkflow` consumes the current draft version and rejects stale confirmation with a stable `draft_version_conflict` error.
- `CreatePreOrderDraftVersion(ctx, DraftInput) (*PreOrderDraft, error)` creates the next immutable version and updates only the pre-order current-version pointer.

- [ ] **Step 1: Add failing tests** for stale version rejection, duplicate confirmation returning the original decision, modified quantity/price creating a new version, and audit fields containing message ID/hash, evidence, model, prompt, and policy versions.
- [ ] **Step 2: Run `go test ./internal/workflows ./internal/api ./internal/repo -run 'Draft|PreOrder|Signal' -count=1`** and confirm the new assertions fail.
- [ ] **Step 3: Add `DraftVersion` to the signal decoder and preserve operator stamping.** A request body operator is always overwritten with the authenticated user.
- [ ] **Step 4: Update the workflow confirmation path.** Load the current draft, compare `DraftVersion`, authorize the operator at API and workflow boundaries, record a decision row, and make repeated confirmation idempotent by `(tenant_id, pre_order_id, draft_version, action)`.
- [ ] **Step 5: Persist modified drafts as new versions.** Store before/after diff and all evidence; never mutate the prior JSON snapshot.
- [ ] **Step 6: Run focused tests and a replay test.** Verify stale cards cannot confirm a newer draft and Temporal replay produces the same state transition.

### Task 3: Deterministic policy engine and strategy activities

**Files:**
- Create: `internal/agentorder/types.go`
- Create: `internal/agentorder/policy.go`
- Modify: `internal/workflows/activities.go`
- Modify: `internal/workflows/store.go`
- Test: `internal/agentorder/policy_test.go`, `internal/workflows/activities_test.go`

**Interfaces:**
- `PolicyInput` contains tenant, group, sender, candidate fields, customer/product/contract snapshots, and correlation ID.
- `PolicyDecision` has `Status` (`VALID`, `NEEDS_CONFIRMATION`, `REJECTED`, `MANUAL_REQUIRED`), `RuleHits`, `InputSnapshot`, `Output`, and `PolicyVersion`.
- `EvaluatePolicy(input PolicyInput) PolicyDecision` is pure and deterministic; `ActEvaluateAgentPolicy` persists its result through `Store`.

- [ ] **Step 1: Write table-driven failing tests** for customer/group ownership, sender authorization, unique SKU and unit conversion, contract/protocol price, payment term, credit limit, warehouse/address, high-value/new-customer escalation, missing fields, and multi-candidate ambiguity.
- [ ] **Step 2: Run `go test ./internal/agentorder -count=1`** and verify the package is missing or tests fail.
- [ ] **Step 3: Implement pure policy evaluation.** Each rule returns a stable rule ID and structured evidence; no rule may call the LLM or invent a master-data value.
- [ ] **Step 4: Register the policy activity.** Persist the exact input snapshot, output, policy version, correlation ID, and decision status; route non-valid outcomes to manual handling.
- [ ] **Step 5: Run policy and activity tests.** Include deterministic repeated evaluation and tenant-isolation cases.

### Task 4: Atomic formal-order creation and workflow outbox dispatcher

**Files:**
- Modify: `internal/repo/orders.go`
- Modify: `internal/workflows/store.go`
- Create: `internal/workflows/workflow_outbox_dispatcher.go`
- Modify: `cmd/worker/main.go`
- Test: `internal/repo/orders_test.go`, `internal/workflows/workflow_outbox_dispatcher_test.go`

**Interfaces:**
- `CreateOrderWithWorkflowOutbox(ctx context.Context, in repo.NewOrder, trace repo.AgentOrderTrace) (*repo.Order, bool, error)` atomically writes order, items, `OrderRequested`, and workflow outbox.
- `WorkflowOutboxDispatcher.Run(ctx)` claims pending rows and calls Temporal `ExecuteWorkflow` with stable `WorkflowID(tenantID, idempotencyKey)`; it marks success, retries with backoff, or dead-letters after the configured maximum.

- [ ] **Step 1: Write failing transaction tests** for commit rollback, duplicate derived order key, event/outbox co-creation, and a dispatcher retry after Temporal start failure.
- [ ] **Step 2: Run `go test ./internal/repo ./internal/workflows -run 'OrderWithWorkflow|Dispatcher|Outbox' -count=1`** and verify failure.
- [ ] **Step 3: Implement the repository transaction.** Use one pgx transaction for `orders`, `order_items`, `order_events`, and `workflow_outbox_events`; return the existing order for an idempotent duplicate without creating a second outbox row.
- [ ] **Step 4: Implement the dispatcher.** Use lease expiry, exponential backoff, stable workflow ID, and `WorkflowAlreadyStarted` as success; preserve error text and payload in dead letters.
- [ ] **Step 5: Start the dispatcher from `cmd/worker/main.go`.** It must run beside the existing SpiceDB projector and Temporal worker, and must stop on context cancellation.
- [ ] **Step 6: Run focused tests plus `go test ./...`.** Verify no code path directly starts Temporal inside the database transaction.

### Task 5: Real Agent parser contract and production WeCom ingress

**Files:**
- Modify: `internal/wecom/intent.go`
- Modify: `cmd/wecom/service.go`
- Modify: `cmd/wecom/routes.go`
- Modify: `internal/config/config.go`
- Modify: `internal/wecom/gateway.go`
- Test: `internal/wecom/intent_test.go`, `cmd/wecom/service_test.go`, `cmd/wecom/routes_test.go`

**Interfaces:**
- `IntentParser.Parse(ctx context.Context, input ParseInput) (IntentParseResult, error)` returns fixed-schema candidates, evidence, model name/version, prompt version, input hash, and parse status.
- `WeComIngress` verifies signature/decryption, normalizes tenant/group/message IDs, persists cursor, and inserts an idempotent inbound message before starting `PreOrderWorkflow`.
- Config adds `AgentEnabled`, `AgentParserProvider`, `AgentPromptVersion`, `WeComIngressEnabled`, retry limit, and checkpoint settings.

- [ ] **Step 1: Add failing parser schema tests** for valid JSON, malformed JSON retry, missing required fields, low confidence, multi-candidate, input hash, and model/prompt version capture.
- [ ] **Step 2: Add failing ingress tests** for bad signature, duplicate `(tenant, group, msg)` delivery, cursor resume, transient retry, and dead-letter after the retry limit.
- [ ] **Step 3: Run the focused tests** and confirm the current deferred-only/mock path fails them.
- [ ] **Step 4: Implement the parser interface and provider adapter.** Keep parser side-effect free; use schema validation and bounded retries; never let parser write orders.
- [ ] **Step 5: Move production message handling into the default build.** Keep `/wecom/mock/message` as a test injection route, add feature flags, and use tenant-specific cursor/checkpoint storage with message hash and redacted raw payload.
- [ ] **Step 6: Run parser, ingress, and route tests.** Verify disabled feature flags fail closed and all accepted messages carry a correlation ID.

### Task 6: Traceability, reconciliation, and manual compensation

**Files:**
- Create: `internal/agentorder/trace.go`
- Create: `internal/repo/dead_letters.go`
- Create: `internal/repo/reconciliation.go`
- Modify: `internal/api/server.go`
- Modify: `internal/config/config.go`
- Test: `internal/agentorder/trace_test.go`, `internal/repo/reconciliation_test.go`, `internal/api/server_test.go`

**Interfaces:**
- `AgentOrderTrace` carries `tenant_id`, `group_id`, `msg_id`, `pre_order_id`, `draft_version`, `order_id`, `workflow_id`, model/prompt/policy versions, and confirmation operator.
- `GET /api/v1/agent/orders/{id}/trace` returns the ordered trace and redacted evidence.
- `POST /api/v1/agent/dead-letters/{id}/retry` and `POST /api/v1/agent/orders/{id}/resume` require privileged authorization and write an audit decision.
- `FindRequestedOrderOrphans(ctx, olderThan)` returns `ORD_REQUESTED` orders without a completed/active workflow outbox row.

- [ ] **Step 1: Write failing trace and reconciliation tests** for complete trace assembly, tenant isolation, redaction, orphan detection, retry authorization, and manual resume audit.
- [ ] **Step 2: Implement trace persistence/query and dead-letter operations.** Keep raw sensitive fields redacted in API output while preserving hashes for audit.
- [ ] **Step 3: Implement orphan scanner and alert payload.** A scanner must identify an order, idempotency key, last outbox attempt, and suggested compensation action.
- [ ] **Step 4: Add API routes and privileged checks.** Every retry/resume is idempotent and records operator, reason, and timestamp.
- [ ] **Step 5: Run focused API/reconciliation tests.** Include a fake clock case for lease expiry and orphan age thresholds.

### Task 7: End-to-end replay, shadow reconciliation, and release gates

**Files:**
- Create: `tests/l2_e2e/agent_ordering_e2e_test.go`
- Create: `tests/l2_e2e/agent_ordering_replay_test.go`
- Modify: `tests/l2_e2e/helpers.go`
- Create: `docs/superpowers/runbooks/agent-ordering-rollback.md`

**Interfaces:**
- E2E helpers inject a WeCom message, inspect the versioned draft, confirm a specific version, drain workflow outbox, and inspect the resulting order/workflow trace.
- Shadow reconciliation compares Agent draft fields with human order fields without changing either truth source.

- [ ] **Step 1: Write failing E2E scenarios** for duplicate messages, parse failure, low confidence, unauthorized sender, stale draft version, duplicate confirmation, order derivation, outbox retry, workflow start failure, complete workflow chain, and Agent-versus-human shadow diff.
- [ ] **Step 2: Run `go test ./tests/l2_e2e -run AgentOrdering -count=1`** and record each missing behavior.
- [ ] **Step 3: Implement test fixtures and replay-safe assertions.** Assertions must prove no duplicate formal order, no unconfirmed order, eventual workflow start or visible dead letter, and complete traceability.
- [ ] **Step 4: Add rollback runbook.** Document disabling Agent ingress, draining/pausing dispatcher, replaying safe outbox rows, restoring from dead letters, and returning to manual WeCom/Feishu fallback.
- [ ] **Step 5: Run the release gate commands.** Run `go test ./...`, migration integration tests, E2E tests, and a shadow reconciliation report; do not enable production Agent traffic until all gates pass.

## Self-review checklist

- [ ] Every design requirement maps to at least one task above.
- [ ] No task reuses the SpiceDB `outbox_events` table for workflow startup.
- [ ] All later interfaces use the exact names and fields introduced earlier.
- [ ] Every step contains concrete files, interfaces, code behavior, and test commands rather than placeholders.
- [ ] The implementation can be delivered in phases: internal order chain, policy/draft audit, real ingress/parser, then shadow and gray release.
