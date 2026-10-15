// Package integration —— 外部依赖接入层（docs/08-external-integration.html）
//
// 本包是系统对外部依赖的唯一接入面，承接两类数据：
//
//  1. 动态数据（Dynamic Data）——外部系统的实时/近实时数据：
//     企业微信会话存档（internal/wecom.Gateway 为现有实现）、
//     未来可能的飞书多维表格等。见 source.go。
//
//  2. 业务配置性数据（Business Config）——文档中要求配置化但历史上
//     硬编码的业务规则：审批催办（24h/48h）、无回单超期（5 天）、
//     修改分类规则（A/B/C/D/E）、自动审批阈值。见 config.go。
//
// 依赖方向（与 docs/01-core-graph.html §2.1 领域层纯度约束一致）：
//
//	integration → domain（复用其纯类型/函数），domain 不感知本包；
//	workflows/api 只依赖本包接口，不感知具体 Provider 实现。
package integration
