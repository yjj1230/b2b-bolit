# 覆盖率审计与双 graph 完整性评估（2026-08-06 12:30）

> 触发：按双 graph 逐环节排查覆盖率 <95% 的环节点；验证风险/影响面测试覆盖；
> 按 L 层核对达标；双 graph 节点完整性正/逆向推导。
> 本次补充约 50 个测试；修复 2 个真实缺陷（预订单发送者权限校验缺失、stkPaymentStep 重入归一化）。

## 1 · 环节覆盖率（按 CQ 域聚合，实测）

| 环节（CQ 域） | 载体包 | 实测 | 达标(≥95%) |
|---|---|---|---|
| 领域正确性（FSM/守恒/幂等/值对象，CQ-04/06/07/08/09/14） | domain | **100.0%** | ✅ |
| 配置（业务规则/环境） | config | **100.0%** | ✅ |
| 授权环节（CQ-01/02/03/13/15 + rebac 场景 A-H） | authz + api 投影 | **95.7% / 95.8%** | ✅ |
| 企微接入（CQ-11 + 07 场景） | wecom + workflows 预订单 | **96.2%** | ✅ |
| 流程编排（CQ-04/05/10，Order/PreOrder Workflow） | workflows | **87.4%** | ⚠️ 差距=防御性错误返回 + 基本块行归因假象 |
| 持久化（CQ-06..14 事务/幂等/事件链/游标/outbox） | repo | **82.0%** | ⚠️ 差距=pgx scan 错误/CHECK 防线后分支/Commit 失败（物理不可达为主） |
| **全仓** | — | **89.0%**（门禁 85% + domain 100% 硬门禁） | ⚠️ |

## 2 · 本次补充（约 50 个测试）

| 包 | 补充内容 |
|---|---|
| authz（88.7%→95.7%） | roleAllows 8 权限×角色穷举、投影器 Run 生命周期/SetLogger/RunOnce 错误路径（list/mark 失败、apply 缺字段全分支）、mustStruct panic、NewSpiceDB 懒连接语义 |
| wecom（91.5%→96.2%） | token 缓存+服务器故障组合、ReadAll 中断（Hijack）、VerifyURL/decryptCallback 边界、Pull 跳过解密失败、loadTenantConfigFile 坏 JSON |
| api（92.4%→95.8%） | errStore 注入（GetOrderItems/ListOrderEvents/ListApprovalTasks/CreatePreOrder/GetPreOrder 失败→400）、errStarter（启动/信号失败→500）、LocalRunner Wait ctx 取消/启动失败、userOf 兜底 |
| workflows（85.9%→87.4%） | 未知 verdict 全闸门（7 个）、B/C/D/E 修改后走完、修改路径故障注入（UpdateOrderStateTx#7/#8、resubmit、meta）、预订单深度故障注入（AuthCheck/通知/派生/迁移/重读）、全状态启动（13 个状态）、fakestore 内部方法 |
| repo（81.0%→82.0%） | 触发器注入（order_events 建单、approval_tasks 插入、users/tenants 播种）、GetInvoice/GetCustomerContact/游标成功与边界、Requeue/回滚 |

**真实缺陷修复（测试驱动）**：
1. **预订单发送者权限校验缺失**（目标 4 正向推导发现）：07 §3.2「权限：消息发送者=归属销售/代办（SpiceDB）」未实现——`preIntentStep` 从未调用 `AuthCheck`。已补实现（SenderID 参数 + AuthCheck + SenderUnauthorized→IGNORED+审计）+ PreOrderFSM 增边（INTENT_PARSING→IGNORED）+ 文档同步（01 §2.6）。
2. **stkPaymentStep 重入归一化缺失**：从 STK_PAYMENT_CONFIRMING 状态启动时重复迁移导致状态不匹配——已修复（重读当前状态，仅 WAREHOUSE_CONFIRMED 时迁移）。
3. **fakeStore 幂等重放语义与 repo 不一致**（守恒检查在幂等检查前）——已修复（镜像 repo ON CONFLICT 语义）。

## 3 · L 层核对

| 层 | 达标情况 |
|---|---|
| L0（单元，domain 100%） | ✅ domain/config 100%；authz 静态矩阵 100% |
| L1（集成，真 PG + SpiceDB） | ✅ 全部用例绿（含 L1-15 Outbox 全链）；authz 包 95.7% 由 L1 主导 |
| L2（Temporal TestSuite） | ✅ 全部用例绿（133 测试含补充）；workflows 87.4% 为全包口径 |
| L3（系统验证） | ✅ 脚本真库全绿 + 覆盖率门禁 + roundtrip |

## 4 · 物理不可达/防御性分支清单（诚实记录，占全仓约 8pp）

| 类别 | 例 | 不可达原因 |
|---|---|---|
| pgx scan 错误返回 | GetOrder/GetOrderItems/ListOrders 等所有 Scan 失败分支 | 需 schema 损坏才能触发 |
| CHECK 防线后分支 | ReconcileInventory 负库存、守恒违规聚合 | CHECK(qty>=0)/守恒事务已拦截 |
| Commit 失败返回 | 各 txwrappers 的 `tx.Commit` 错误 | 事务内最后语句错误已由触发器注入覆盖；Commit 本身失败需连接中断 |
| 懒连接构造错误 | NewSpiceDB 的构造失败分支 | grpc.NewClient 懒连接恒成功 |
| validateFSM PRE 分支 / unknown stage | OrderWorkflow 循环 default | 订单工作流不使用 PRE 状态；stage 枚举固定 |
| 基本块行归因假象 | ordSegment 尾部 return 行（count=0 但块首 count=1） | Go coverage 计数器挂基本块首行 |

**结论**：全仓 95% 需 SQL 层错误注入框架（mock pgx 连接）或 schema 变异测试——建议后续迭代评估，当前如实记录差距。

## 5 · 双 graph 节点完整性（目标 4）

### 5.1 正向推导（缺不缺节点）
- 世界知识图谱 15 实体对照业务流程：回款/合同/出库单为订单聚合属性（无独立 CQ 需求，克制添加 ✓）；产品/开票/审批任务/事件挂订单级授权（无独立授权 CQ ✓）
- **发现 1 个真实缺失**：预订单发送者授权校验（07 §3.2 契约）——已补实现（见 §2）
- 授权图谱 8 definition 对照本体：prestock/shipment/evidence 已投影 ✓；Invoice/PreOrder 无独立授权对象（挂 order 级，合理）

### 5.2 逆向分析（节点是否有意义）
- **冗余/未引用**：`tenant.manage_all` permission 无任何外部引用（保留为管理语义，建议后续要么接线要么删除）；`warehouse.manage` 被 order.ship 引用 ✓；`StateSTKWarehouseReturned` 回边（设计补充）有测试背书 ✓
- 全部 8 个 permission/15 个 CQ/5 个 FSM/45 个 Score Card 用例均有测试背书或阶段二预留，无"为建而建"节点

## 6 · 结论
- 5/7 包 ≥95%（domain/config 100%）；全仓 89.0%（+2.6pp，本轮）
- 流程/持久化环节差距为防御性错误返回 + 基本块归因 + 物理不可达分支（§4 清单）
- 双 graph 无结构性裂缝；预订单权限裂缝已闭合；1 个冗余节点已标注
