# pgAPI D-006 验证证据包 · 2026-08-18

环境：`10.0.2.160:/data/home/laurence/b2b_pilot_v3`，分支 `graph_guide`，
验证起点 HEAD `74fd09b0d75ba2d0a42facef954a4242714a70e4`（D-006 提交 `57a5c6f` 的 docs-only 后继提交）。

机制状态：ECM 1.0.0 仍为 **PROPOSED**（BOOTSTRAP_DIAGNOSTIC）；
本证据包只记录验证事实，不构成 ECM PASS 声明，也不构成 handoff ACCEPTED（ECM-REC-004）。

## 文件清单

| 文件 | 内容 |
|---|---|
| go_build_all.txt | `go build ./...` 输出（exit 0） |
| go_vet_all.txt | `go vet ./...` 输出（exit 0） |
| test_l2.txt | `make test-l2`（workflows/api/wecom，exit 0；修复后） |
| test_l1.txt | `make test-l1`（真实 PG + SpiceDB，exit 0；修复后） |
| test_tests_l1.txt | `make test-tests-l1`（HTTP+DB 黑盒，exit 0） |
| test_tests_l2.txt | `make test-tests-l2`（E2E + DemoMemoryProvider，exit 0） |
| test_tests_l3.txt | `make test-tests-l3`（系统验证，exit 0） |
| test_all_packages.txt | `go test -count=1 ./...`（exit 0） |
| cover_gate.txt | `make cover` 门禁尾部：total 85.4%、domain 100%（exit 0） |
| migrations_001_007_apply.txt | 全新一次性库按序应用 001–007 全部成功 |
| pgapi_service_smoke.txt | 本地一次性 pgAPI 服务（真实 pgAPI 源码 + 最小 rhcrm schema）curl 读写 smoke |
| go_client_pgapi_smoke.txt | `internal/pgapi` Go 客户端对真实 pgAPI 服务的 CRUD 契约验证 |
| backfill_dryrun_match.txt | `scripts/pgapi_backfill` dry-run：4/4 MATCH，exit 0 |
| backfill_apply_idempotent.txt | `-apply` 幂等执行：4 mappings committed，exit 0 |
| backfill_dryrun_unmatched.txt | 未匹配 legacy ID 时 dry-run：FAIL 清单 + 不写数据，exit 2 |
| backfill_apply_verify.txt | apply 后 pgapi_entity_mappings 与 7 类引用列改写结果 |

## 可复现方式

- 测试 PG：容器 `pilot-test-pg`（postgres:16-alpine，127.0.0.1:55432）。
- 一次性库：`b2b_pgapi_test`（最小 rhcrm schema + 种子数据）、`b2b_backfill_test`（migrations 001–007 + legacy/流程种子）。
- pgAPI 服务：`/data/home/laurence/pgAPI/external/pgapi` 源码，uvicorn 127.0.0.1:8010，
  `PGAPI_API_KEY=test-key-123`，`CARH_DB_*` 指向 `b2b_pgapi_test`。
