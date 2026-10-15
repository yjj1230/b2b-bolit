// Package shadowguard —— 影子轨启动守卫（docs/v0.1/12-shadow-full-flow.html 附录 A · A-01）
//
// B2B_SHADOW_MODE=true 的六个二进制（shadow-api / shadow-worker / shadow-feed /
// shadow-ingest / shadow-audit / shadow-migrate 同构校验）在启动时 fail-fast：
// 任一隔离项违反 → 非零退出 + 明确日志（S-07 负向启动测试口径）。
// 守卫是「默认拒绝」工程护栏：常规误配置（照抄生产 DSN/队列/namespace/channel）全部拦下，
// 残余风险仅剩「持有生产凭据者蓄意绕行」，由权限与审计兜底（A.4）。
package shadowguard

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"b2b_pilot_v3/internal/config"
)

// 影子轨隔离基准（与 12-shadow-full-flow §7 环境变量一一对应；唯一准入值）。
const (
	ShadowNamespace  = "b2b-shadow"
	ShadowTaskQueue  = "b2b-shadow-order"
	ShadowDBName     = "b2b_shadow"
	ForbiddenDBToken = "b2b_pilot" // 生产业务库名片段：命中即拒绝（A-01 T1/T2）
	// ShadowTenant 影子租户（A-03：chang-an-shadow 后缀，与 A 轨 chang-an 空间隔离；
	// 生产库写哨兵按 tenant ILIKE '%shadow%' 校验零污染）。
	ShadowTenant = "chang-an-shadow"
)

// Validate 校验影子配置；合法返回 nil，非法返回聚合错误（全部违规项一次性列出）。
func Validate(cfg config.Config) error {
	if vs := Violations(cfg); len(vs) > 0 {
		return fmt.Errorf("shadow guard rejected startup: %s", strings.Join(vs, "; "))
	}
	return nil
}

// Violations 返回全部违规项（S-07 负向测试逐项断言用）。
func Violations(cfg config.Config) []string {
	var out []string
	if !cfg.ShadowMode {
		return []string{"B2B_SHADOW_MODE must be true: shadow binaries refuse to run without explicit shadow opt-in"}
	}
	// A-01 · 1：DSN 库名必须 = b2b_shadow；命中生产库名/片段直接拒绝（T1 照抄主 compose 场景）。
	if v := dsnViolation(cfg.PGDSN); v != "" {
		out = append(out, v)
	}
	// A-01 · 2：Temporal namespace（T3 影子 worker 抢跑生产 namespace 场景）。
	if cfg.TemporalNS != ShadowNamespace {
		out = append(out, fmt.Sprintf("B2B_TEMPORAL_NS must be %q, got %q", ShadowNamespace, cfg.TemporalNS))
	}
	// A-01 · 3：task queue（T3 影子 worker 抢跑 b2b-order 生产队列场景）。
	if cfg.TaskQueue != ShadowTaskQueue {
		out = append(out, fmt.Sprintf("B2B_TASK_QUEUE must be %q, got %q", ShadowTaskQueue, cfg.TaskQueue))
	}
	// A-01 · 4：零出站（T5 经影子入口创建真实飞书订单 / 发客户消息场景）。
	if !strings.EqualFold(cfg.WeComProvider, "mock") {
		out = append(out, fmt.Sprintf("B2B_WECOM_PROVIDER must be mock in shadow mode, got %q", cfg.WeComProvider))
	}
	// A-01 · 5：零回写（T5 桥接开启场景）。
	if cfg.FeishuBridgeEnabled {
		out = append(out, "B2B_FEISHU_BRIDGE_ENABLED must be false in shadow mode (零回写)")
	}
	// A-01 · 6：SpiceDB 影子实例白名单（T4 Outbox projector 污染生产授权图场景）。
	if v := spicedbViolation(cfg.SpiceDBAddr); v != "" {
		out = append(out, v)
	}
	return out
}

// ValidatePrincipals 影子 API 身份白名单（S-铁1：仅内部白名单可访问）。
func ValidatePrincipals(principals string) error {
	if strings.TrimSpace(principals) == "" {
		return fmt.Errorf("B2B_SHADOW_PRINCIPALS must be set for shadow-api (S-铁1 身份白名单)")
	}
	for _, part := range strings.Split(principals, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), ":", 2)
		if len(kv) != 2 || kv[0] == "" || kv[1] == "" {
			return fmt.Errorf("B2B_SHADOW_PRINCIPALS malformed entry %q (want userID:ROLE)", part)
		}
	}
	return nil
}

// dsnViolation 解析 DSN 并校验影子库名。
func dsnViolation(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		return fmt.Sprintf("B2B_PG_DSN must be a parseable postgres URL with host, got %q", dsn)
	}
	db := strings.TrimPrefix(u.Path, "/")
	lower := strings.ToLower(db)
	if strings.Contains(lower, ForbiddenDBToken) {
		return fmt.Sprintf("B2B_PG_DSN database %q hits production token %q — refusing to run", db, ForbiddenDBToken)
	}
	if db != ShadowDBName {
		return fmt.Sprintf("B2B_PG_DSN database must be %q, got %q", ShadowDBName, db)
	}
	return ""
}

// spicedbViolation 校验 SpiceDB 地址指向影子实例（回环测试地址或含 shadow 标识的主机名）。
func spicedbViolation(addr string) string {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return "B2B_SPICEDB_ADDR must be set to a shadow instance address"
	}
	if host == "127.0.0.1" || host == "localhost" || host == "::1" {
		return ""
	}
	if strings.Contains(host, "shadow") {
		return ""
	}
	return fmt.Sprintf("B2B_SPICEDB_ADDR %q is not in the shadow allowlist (host must contain shadow or be loopback)", addr)
}
