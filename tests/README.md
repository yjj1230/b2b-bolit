# tests/ —— L 分层测试（HTTP API 业务流）

按 `docs/03-testing.html` 的 L 分层组织的测试目录，聚焦**除企微之外的 HTTP API
业务流**：企微（`internal/wecom` + `cmd/wecom`）按设计是建构于底层 API 之上的
特定应用转接层，业务能力以 HTTP API（`cmd/api`）为规范面。

## 目录与 L 分层映射

| 目录 | 分层 | 内容 | 依赖 | 运行 |
|---|---|---|---|---|
| `l0_unit/` | L0 单元 | 导出 API 黑盒契约：5 个 FSM 迁移表 / 守恒断言 / 幂等键规范 / 回单四分流 / 接入层默认值 / StaticChecker 对照表 | 无（纯 Go） | `go test ./tests/l0_unit/...` |
| `l1_integration/` | L1 集成 | HTTP API × 真实 PostgreSQL：幂等唯一 / 跨租户隔离 / 分域事件链 / 凭证只追加（PK 防线）/ 参数校验 | 真 PG | `make test-tests-l1` |
| `l2_e2e/` | L2 端到端 | HTTP API 全业务流：款到全链 → CLOSED / 非款到 / 董事长 C-D / 部分发货 / 修改 A-E / 拒绝 / 预订单全链 / 凭证链 / BFF 投影 / **W5 企微回调转接 / W6 S1 侧边栏** | 真 PG（工作流进程内执行） | `make test-tests-l2` |
| `l3_system/` | L3 系统验证 | HTTP 全链 + 系统脚本驱动：seeddata 幂等 / reconcile 四守恒 / audit 审计链 / concurrency 并发守恒 | 真 PG（独立 DB） | `make test-tests-l3` |
| `testkit/` | 共享装配器 | 真实装配：`api.NewServer` + `api.LocalRunner`（进程内真实 OrderWorkflow/PreOrderWorkflow，Temporal TestSuite 环境）+ 真实 PG + StaticChecker + wecom mock 出站 | — | 被上列套件引用 |

## 与现有 internal/ 测试的关系（为什么「不挪」）

`internal/domain|repo|authz|workflows|api` 下的测试是**白盒**测试（同包可见未导出
符号），且是覆盖率门禁的载体（`make cover` 对 `./internal/...` 计覆盖，domain 100%
硬门禁）。整体迁移到 `tests/` 会破坏门禁与白盒保真度，故保留原位；`tests/` 承担
**黑盒契约 + HTTP API 视角**的 L 分层套件，两者互补、零重复：

- internal 白盒穷举：FSM 全迁移穷举、每句 SQL 失败注入、TestSuite 虚拟时间 Timer 流
- tests 黑盒契约：文档契约防漂移、HTTP 真实装配端到端、系统脚本驱动

## 运行

```bash
make test-tests-l0        # 纯单元，无依赖
make test-tests-l1        # 需 PILOT_TEST_PG_DSN（make up 起测试 PG）
make test-tests-l2        # 同上
make test-tests-l3        # 需 B2B_L3_PG_DSN（默认 55432/b2b_pilot）
make test-tests           # l0 + l1 + l2 + l3
```

L1/L2 每个用例独立重置 schema（迁移 001-005 + 种子世界），PG 不可用时按
`internal/repo` 同语义降级 Skip。

## 企微 L 层（W 系列）测试

企微 = HTTP API 之上的转接层（docs/07 §1/§4.1），其测试按「W」编号与 L 层并轨：

| 编号 | 内容 | 位置 | 依赖 | 覆盖/风险/影响评估 |
|---|---|---|---|---|
| W1 | 回调验签/解密（VerifyURL/VerifyCallback/两段解密） | `internal/wecom/w1_test.go` | 无 | docs/09 §2「企微 S3」行 |
| W2 | 出站卡片/通知（审批/预订单/状态/回单异常） | `internal/wecom/wecom_test.go` | 无 | 同上 |
| W3/W4 | 真实企微环境人工验收（回调 URL 配置/真实消息流） | Phase 3 人工清单（07 §10.3） | 真实企微测试企业 | 07 §10 部署指南 |
| W5 | 回调 → HTTP API 动作转接（approve/reject/preorder confirm/ignore/modify + 幂等回执） | `tests/l2_e2e/wecom_callback_e2e_test.go` | 真 PG | docs/09 §2「企微 S3」行 |
| W6 | S1 侧边栏服务端链路（jsapi 双签名/OAuth exchange/context_token 校验） | `tests/l2_e2e/wecom_s1_e2e_test.go` | 真 PG | docs/09 §2「企微 S1」行 |

一键运行（mock 模式全覆盖 + 暂缓段回归）：

```bash
make test-wecom
```

- 意图识别自动下单（`-tags deferred`）属暂缓段（07 §3.0），默认构建不含、默认测试不跑；`go test -tags deferred ./internal/wecom/...` 供恢复阶段回归。

## 信号驱动说明（L2）

HTTP 信号 = 人工闸门（02 §2.1）：`POST /api/v1/orders/{id}/signal/{name}`。
用例在每道闸门前先 `WaitOrderState` 轮询期望状态再投递，消除本地运行器的
异步竞态。

## Timer 语义（LocalRunner 虚拟时间）

`api.LocalRunner` 内嵌 Temporal TestSuite 环境，其虚拟时钟在工作流阻塞时自动触发
最短 Timer（auto-fire），催办循环会快速烧穿 10y 运行预算。因此：

- 套件默认策略 = `integration.NewDemoPolicy`（10s 虚拟时长）：催办每轮仅推进
  10s 虚拟时间，持续等待约 52 分钟才烧穿预算——交互式演示（cmd/api 本地模式）与
  测试均可稳定使用；等待期间催办通知 ~10 次/秒（mock 出站，无害）。
- Timer 驱动路径在 E2E 中按「自动触发」语义覆盖：回单超期 → 例外审核（分流③）、
  预订单升级 → 降级确认；「补回单立即开票」（分流②）等依赖精确时间点的路径由
  `internal/workflows` TestSuite（L2-07/11/13 虚拟时间控制）覆盖。
- 生产时长语义（24h/48h/5d）见 `internal/integration/static.go` + `docs/08`。
