package main

import (
	"context"

	"b2b_pilot_v3/internal/domain"
)

type preOrderStarter interface {
	StartPreOrderWorkflow(context.Context, string, string, string) (string, error)
}

// wecomPreKey 预订单幂等键（pre:<tenant>:<group>:<msgid>:v1，架构 §5 键规范）。
// 定义在无构建标签文件中：默认与 deferred 两条构建路径均需可见。
func wecomPreKey(tenantID, groupID, msgID string) (string, error) {
	return domain.PreOrderKey(tenantID, groupID, msgID, "v1")
}
