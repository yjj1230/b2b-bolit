# Agent 下单回滚手册

1. 将 `B2B_AGENT_ENABLED=false` 和 `B2B_WECOM_INGRESS_ENABLED=false` 发布到 Agent/企微服务，保留人工预订单和旧桥接入口。
2. 暂停 worker 中的 workflow outbox dispatcher；不要删除 `workflow_outbox_events`、`dead_letter_events` 或订单事件。
3. 查询 `workflow_outbox_events` 中 `PENDING`、`PROCESSING`、`FAILED` 和 `DEAD` 记录，确认每条记录对应的订单、幂等键和最后错误。
4. 对已确认但未启动的订单，优先恢复 dispatcher；Temporal 返回已存在的稳定 WorkflowID 时按成功处理，避免人工重复建单。
5. 对超过重试阈值的记录，通过受控补偿入口重试或人工启动对应 `OrderWorkflow`，并记录操作人、原因和时间。
6. 恢复 Agent 前，先重放重复消息、旧卡片版本、重复确认、Temporal 启动失败和租户隔离测试；影子对账无差异后再按租户灰度开启。
