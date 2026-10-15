# ECM 机制版本 CHANGELOG

本文件记录 ECM 机制版本的差异与迁移提示（§07 版本边界）。已发布版本不可原地覆盖；
当前生效版本由 `project/policies/mechanism/current.yaml` 指针选择，不能靠无版本文件或 mtime 猜测。

## 版本状态总览

| 版本 | 状态 | 日期 | 备注 |
| ---- | ---- | ---- | ---- |
| 0.9.3 | 虚拟前序 | — | 无真实制品；仅作为 `rollback_to` 占位，见 migrations/ECM-0.9.3-to-1.0.0.yaml |
| 1.0.0 | PROPOSED | 2026-08-13 | 本次 bootstrap 落库；ECM-ACT-001 未满足，未激活 |

## [1.0.0] — 2026-08-13（bootstrap 首版）

### 修订 2026-08-13T09:05Z（复盘修复，PROPOSED 未发布状态内修订；事件 EVT-000006..0012）

- **规范源补转写**：evidence_policy 组件补 §06 五种入口差异化路由（spec/docs/demo/代码/测试）、
  快循环 7 步、慢循环 OBSERVE..ACTIVATE 六步与 question_packet 契约（原转写遗漏 §06 核心内容）。
- **M1 退出条件落库**：新增 `project/baselines/schema.yaml`（§09 M1 交付物"基线 schema"）；
  calibration-cases.yaml 新增 `m1_exit_validation`（SIM-01..06 → 机制元素映射，
  机械证明"能够表达 SIM-01..06"）。
- **根 HTML**：hero-meta"当前载体"改为"历史载体（bootstrap 提案）"，消除与规范源指针声明的矛盾。
- **handoff 契约补齐**：manifest 补 bootstrap_mode=true / mechanism_status=PROPOSED；
  change_cursor 改为事件流游标语义（EVT-000012，§10 模板）；acceptance.yaml 补
  bootstrap_mode=true（A.8 required-boolean）。
- **事件流补记**：为上述所有变化追加 ArtifactChanged 事件（§11 每次变化必须产出）；
  上一轮对根 HTML 的修改遗漏事件，本次一并补记（EVT-000009）。
- digest 级联更新：ECM-1.0.0.yaml spec_digest `a0418136` → `e8f561b2`；
  evidence_policy 组件 `c119d26e` → `9a6ef66c`（current.yaml / project.yaml 已同步）。

### 变更（相对 0.9.3 虚拟前序）

- **对象/关系/状态**（§04）：定义六种对象（source/claim/decision/artifact/evidence/baseline）、
  九种关系、三组正交状态（semantic / implementation / verification）。
- **权威矩阵**（§05）：六类问题的首要权威/次级证据/不可替代项；九步裁决算法；权威公式；
  五层一致性。
- **双循环**（§06）：INGEST→NORMALIZE→DIFF→IMPACT→ROUTE→VERIFY→BASELINE；
  每次变化生成 ArtifactChanged 事件与局部收敛。
- **版本化机制**（§07）：双制品（YAML 规范源 + HTML 指南）；current.yaml 指针；
  policy component digest 局部绑定。
- **Handoff 协议**（§10 + 附录 A.6–A.8）：OFFERED→ACCEPTED/CONDITIONAL/REJECTED；
  acceptance 机器契约 ECM-REC-001..004。
- **符合性协议**（附录 A）：A.1 规范效力、A.2 禁止推断、A.3 术语、A.3.1 规范化与摘要
  （ECM-CANON-001..006）、A.4 状态机、A.5 确定性发现、A.9 失败码、A.12 激活门槛
  （ECM-ACT-001..004）。

### 迁移提示

1. 本次为 bootstrap：仅新增 `project/` 控制面结构，**未移动/重命名任何现有 spec/docs/代码**。
2. `current.yaml.status` 为 `PROPOSED`：附录 A.5 步骤 06 要求 `ACTIVE` 才可进入
   MECHANISM_VALIDATING；未激活前 Agent 按 A.4 进入 `BOOTSTRAP_DIAGNOSTIC`，只做盘点与已授权迁移。
3. 激活路径（§07 “规则从事故到生效”）：补齐 ECM-ACT-001 四件套（schema/canonicalizer/
   测试向量/validator）→ 影子运行 → 人工批准 → 原子更新 current.yaml 与 project.yaml。
4. 已知文档内部不一致（发布缺陷待修）：§07 policy manifest 示例的
   `calibration.corpus: project/policies/calibration-cases.yaml` 与同节目录树
   `project/policies/mechanism/calibration-cases.yaml` 不一致；本版按目录树落库，
   ECM-1.0.0.yaml 的 calibration.corpus 指向 `project/policies/mechanism/calibration-cases.yaml`。
   按 ECM-AUTH-003，若与 HTML 指南冲突应报告 MECHANISM_CONTENT_CONFLICT，此处已在规范源中显式落定。

## 未发布草稿

（无）
