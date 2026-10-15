# HOF-20260813-001 — Bootstrap Handoff 摘要

## 交接方
- Agent: reasonix-executor（bootstrap 模式，§10 例外）
- 时点: 2026-08-13T08:37:00+00:00
- 基点: branch `graph_guide` @ commit `e42e3ea05fbad79a5793e3464173df8d92c69df3`

## 本次完成（M0 + M1 + 机制双制品）
1. **M0 冻结现状索引**：`project/views/artifact-manifest.yaml` 登记全部 213 个现有制品
   （artifact_id=相对路径、kind、sha256 digest、git 时间、推定 owner），记录 OBS-01..06
   当前差异（用例总表 4 步向导 vs 单页表单；物流小程序 vs 图片发群；签收凭证前提冲突；
   221 vs 180 测试计数；demo 观察时点；wecom 未跟踪）。未移动任何文件。
2. **M1 控制面骨架**：`project/` 目录树（claims×4 域占位 / decisions / conflicts /
   baselines / policies/mechanism / events / handoffs / views）。
3. **机制双制品**（§07）：`ECM-1.0.0.yaml`（规范源：ontology/authority_matrix/
   state_machine/evidence_policy/handoff_policy 五组件 + 摘要）+ `ECM-1.0.0.html`
   （根 HTML 冻结副本）+ `current.yaml` 指针（**status=PROPOSED**，bootstrap=true）。
4. **项目绑定**：`project.yaml`（mechanism id/version/status/pointer/spec_digest/
   policy_component_digests、baseline=NONE）+ `ontology.yaml` 受控词表。
5. **校准与迁移**：`calibration-cases.yaml`（SIM-01..06）、`CHANGELOG.md`、
   `migrations/ECM-0.9.3-to-1.0.0.yaml`（虚拟前序）。
6. **自检器**：`scripts/ecm_selfcheck.py`（--fill 回填组件摘要 / --check 校验
   A.5 机械部分 + digest 三处一致 + 事件流 + handoff 契约）。
7. **决策库**：`project/decisions/D-001..D-005`。

## 当前状态
- 机制 **PROPOSED 未激活**（ECM-ACT-001 硬约束）；后续 Agent 运行 A.5 将进入
  BOOTSTRAP_DIAGNOSTIC，只可做只读盘点与已授权控制面迁移，不得用 ECM 门禁宣布 PASS。
- 自检 29/29（含 claims 库 26 条合法性检查）。
- 根 HTML 保留为 bootstrap 提案（已加指针声明，正文未改）。

## M2 首批进展（2026-08-13，EVT-000014..0019）
- 四个首选声明簇首批 **26 条 claims** 落库：交互形态 8（ordering）、签收凭证链 7
  （fulfillment）、群权限交集 6（authorization）、企微交互 5（wecom-interaction）。
- **SIM-01/SIM-02 活案例已落位**：CLM-RECEIPT-CHAIN-001 vs 010（评审 XS6-01 否定
  "客户发签收照片"前提，demo/用例总表仍按旧前提——CHALLENGED 保留审计）；
  CLM-ORDER-ENTRY-UI-001 记录用例总表 XS2 仍写"4 步向导"（STALE_VIEW，待 OI-011 同步）。
- **demo/生产边界标注**：project/views/demo-boundary.yaml（17 场景三态，
  real_wecom 全部 UNKNOWN，防"演示可点=已交付"误读）。
- 全部 R2/R3 claims 有 owner/source/decision 状态（M2 退出条件首批达标）。

## 未完成（详见 open-items.yaml）
- 机制激活四件套与 10 类激活测试（OI-001/002）
- M2 业务 claims 抽取（需 owner 参与，OI-003/004/005/007）
- 用例总表同步（OI-006）、文档不一致裁决（OI-010）
- 首次真实迭代验证机制可操作性（OI-009）

## M2 修订（2026-08-14，基于最新原始需求 spec/Demo意见收集_文字版.html，EVT-000020..0024）
- **前提冲突闭合**：CLM-RECEIPT-CHAIN-010（客户发签收照片）→ **SUPERSEDED**（demo 已删 XS6、
  意见收集第 6 项明确删除）；新增 CLM-RECEIPT-CHAIN-011（真实链：司机→仓管→凭证机器人，
  运满满直传）——SIM-01 活案例走完 CHALLENGED→SUPERSEDED 生命周期。
- **claims 26 → 34 条**：新增 8 条（QUERY-HOME/FORM-FIELDS/REJECT/CONFIRM/APPROVAL-CHAIN/
  SEND-CUSTOMER/CUSTOMER-VIEW/CHAIN-011）；修订 ASSISTANT-001（4 步向导→单页长表单）、
  ACL-001/DELIVERY-001/PRICE-001 等。
- **demo 边界**：17→16 场景（XS6 入 removed_scenes 审计）；XSZL1/SW2/XS1/XSZG1 标注更新。
- **用例总表 stale 范围扩大**（OI-011）：XS2 4 步向导、XS6 未删、XSZL1 4 步向导、无订单 Home。
- 自检 29/29（claims 34 条校验）；事件流 24 条。

## D-006 迭代（2026-08-17，用户裁决的 bootstrap exception）
- 数据库访问二分：流程引擎数据保留 internal/repo 直连 PG；订单数据实体经 internal/orderdata → pgAPI。
- 已落地：internal/pgapi 客户端（products/customers 查增改 + purchase-contracts 只读）、orderdata Provider、workflows 主数据活动改造、migrations/007 去 FK、pgapi_backfill 回填工具、文档同步。
- 未闭合：go build/test 未执行（bash 不可用）；真实旧数据 ID 回填未执行（需现场 pgAPI 数据与人工确认）。见 OI-014/OI-015。

## D-006 验证轮（2026-08-18，接手 Agent 在 10.0.2.160 完成）
- 首次真实编译：go build ./... 与 go vet ./... 全绿；修复 3 处从未编译/从未运行的测试缺陷
  （F1 activities_test `:=` 编译错误；F2 幂等冲突断言方向写反并补正向重放用例；F3 outbox 期望 4→5）。
- 测试矩阵全绿：make test-l0 / test-l1（真实 PG+SpiceDB，migrations 001–007 按序成功）/ test-l2 /
  test-tests-l1 / test-tests-l2 / test-tests-l3 / go test ./... / wecom deferred / 新增包 -race。
- 覆盖率门禁：84.1% → 85.4%（补 internal/pgapi 与 internal/orderdata 单测），domain 100% 硬门禁通过。
- 真实 pgAPI 契约：一次性后端（真实 pgAPI 源码 + 最小 rhcrm schema）验证 search/get/POST/PATCH/
  DELETE-400 与 Go 客户端 CRUD 全对；用户裁决“本地一次性验证已足够”，真实 CARH 环境由用户/现场执行。
- pgapi_backfill：一次性库验证 dry-run 4/4 MATCH、-apply 改写 7 类引用列、未匹配 exit 2 零写入；
  真实数据 dry-run 与生产 apply 仍待用户安排（OI-014/OI-015 更新）。
- 控制面修复（用户授权）：D-002~D-005 决策 YAML 语法修复；本包 spec/open_items/evidence digest 与
  change_cursor 同步；EV-007~EV-010 证据入 evidence-index。
- 报告与原始证据：docs/history/pgapi-d006-verification-20260818.html 与
  docs/history/evidence/pgapi-d006-20260818/。
- 诚实边界：ECM 仍 PROPOSED（BOOTSTRAP_DIAGNOSTIC）；不宣称 ECM PASS；handoff 仍 OFFERED、
  accepted=false（ECM-REC-004 禁止 bootstrap 写 ACCEPTED）。


## RHCRM 数据源架构 × v0.1 裁决（2026-08-18，D-007）
- 依据更新后的 spec/RHCRM-订单数据源架构说明.html（070c8d4）：PG 购销合同表为唯一订单源
  （存量 7,485 + 三方 1,299 = 8,784 行已同步）；企微订单经 pgAPI POST purchase-contracts 写入。
- 用户裁决：Q1=B v0.1 只做合同表只读可见，写路径下一迭代启用；Q2=U2 状态回写用 apply_no upsert；
  Q3=P3 发布前把备库提升为可写副本（演练库）运行。
- 新增开放项 OI-016：pgAPI 补 25 个合同映射字段 + apply_no 唯一 upsert；b2b 补合同写类型与转换。
- 影响评估：spec/RHCRM-订单数据源架构说明-v0.1影响评估.html。

## 销售助理全场景 + 订单详情页（2026-08-19，EVT-000039/000040）
- docs/demo 新增 XSZL2 客户群入口代下单场景，销售助理角色场景 1→2，demo 覆盖 17/17。
- 新增 docs/ui/assistant-spec.html（XSZL1/XSZL2，PC + 移动端）与 docs/ui/order-detail-spec.html
  （全字段/剥价，PC + 移动端）；docs/ui/index 增补入口；bridge_ui 增加订单详情查看。
- claims 34→36：CLM-ORDER-ENTRY-ASSISTANT-002（群入口归属锁定）、CLM-ORDER-DETAIL-001（订单详情投影）。
- docs/用例总表.html 以 demo 为最高裁决完成同步：XS6 移除、单页长表单、XSZL2、订单 Home/发给客户、
  订单详情通用页、PC/移动切换；OI-006/OI-011/OI-017 置 DONE。
- demo-boundary 更新 17/17；open-items OI-017 完成；selfcheck 29/29 通过。

## official 企微接线 + 现场验收单（2026-08-19，EVT-000042/000043）
- 新增生产可用销售助理页面 `GET /assistant`（工作台代下单 + 客户群入口代下单，PC/手机自适应）。
- official 企微代码接通：JS-SDK 双 ticket + 备案域名白名单、OAuth code→userid、群上下文绑定、
  template_card、`SendGroupCard`（appchat/send）、`cmd/api` 注入 `GroupSender/JSAPI/Identity`。
- 测试/生产配置注入：`config/wecom-tenant.test.example.json` / `config/wecom-tenant.prod.example.json`，
  docker-compose 通过 `B2B_WECOM_TENANT_CFG_FILE` + `B2B_WECOM_CONTEXT_SECRET` 分别注入。
- 新增 `staff_scene_v0.1.html` 现场验证与处理单；`v0.1发布过程跟踪.html` 线 W ≈8%→36%，
  T4.1-T4.5 完成度/要点刷新；open-items 新增 OI-018。
- 现场待办：真实租户 JSON 填值、群绑定 SQL、V-A..V-F 逐项验收并回填跟踪页；旧系统 T-01 仍需在
  tripartite-approval 仓库执行。

## 给接手方的建议首个动作（manifest.next_recommended_action）
现场按 staff_scene_v0.1.html 填测试/生产租户配置并执行 V-A..V-F 验收；
代码侧 official 接线与 H5/PC 页面已提交。

## 诚实声明
- 本摘要与全部证据由同一 Agent（bootstrap 执行者）生成，**不构成独立接手验证**（A.10 第 6 条）。
- acceptance.decision 保持 NOT_APPLICABLE，由后续独立接手方填写（ECM-REC-004）。
