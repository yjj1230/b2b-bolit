// Package config —— 环境变量配置（docs/02-architecture.html §2.1）
package config

import (
	"os"
	"strconv"
	"time"
)

// Config 应用配置（全部来自环境变量，本地默认值便于测试与演示）。
type Config struct {
	// HTTP
	HTTPAddr string
	// PostgreSQL
	PGDSN string
	// pgAPI（订单数据实体：products/customers/purchase-contracts 经 HTTP 访问）
	PGAPIBaseURL string
	PGAPIAPIKey  string
	PGAPITimeout time.Duration
	// PGAPIArchiveEnabled 测试特化：新客户下单后直写备库 pgAPI（B2B_PGAPI_ARCHIVE_ENABLED=true）。
	// 生产环境默认关——生产有 rhcrm-sync（多维表→PG），新客户走「写多维表→同步→PG→老客户读取」原链路，
	// 直写反而会被生产同步软删（权威源=多维表）。
	PGAPIArchiveEnabled bool
	// SpiceDB
	SpiceDBAddr string
	SpiceDBKey  string
	// Temporal
	TemporalAddr string
	TemporalNS   string
	// 企微通道（07-wecom-integration.md §6 多备案切换；provider 选型 = wrapper/plugin 隔离点）
	WeComProvider  string // mock | official | thirdparty
	WeComTenantCfg string // 租户级配置 JSON 文件路径（多备案；测试/生产环境注入不同文件）
	// WeComContextSecret S1 上下文令牌 HMAC 密钥（测试/生产环境注入不同值）
	WeComContextSecret string
	// Agent 下单和真实企微入口开关；默认关闭，灰度时按租户/环境启用。
	AgentEnabled           bool
	AgentParserProvider    string
	AgentPromptVersion     string
	AgentLLMBaseURL        string
	AgentLLMAPIKey         string
	AgentLLMModel          string
	AgentLLMTimeout        time.Duration
	WeComIngressEnabled    bool
	WeComIngressMaxRetry   int
	WeComIngressCheckpoint string
	// 飞书三方审批下单桥接（docs/发布计划v2 第 1 次发布；A1 代理 OAuth 方案）
	// FeishuBridgeBaseURL 指向旧系统（tripartite-approval）；AppID/RedirectURI 用于构造授权 URL。
	FeishuBridgeBaseURL     string
	FeishuBridgeAppID       string
	FeishuBridgeRedirectURI string
	FeishuBridgeEnabled     bool
	// 回调动作转接目标（07 §4.2）：cmd/wecom 回调 → HTTP API 服务地址（企微 = API 之上的转接层）
	WeComAPIAddr string
	// 回单阈值（业务规则默认值：无回单超 5 天进例外，04-development-plan.md §0 待确认项默认值）
	ReceiptDays int
	// 催办阈值（小时）：24h 提醒 / 48h 升级
	RemindHours   int
	EscalateHours int
	// 审批人路由（ROL-01：配置化替换 workflows/order.go 硬编码 u-*）
	AssigneeSupervisor       string
	AssigneeChairman         string
	AssigneeOP               string
	AssigneeAccounting       string
	AssigneeSales            string
	AssigneeWarehousePrefix  string
	AssigneeWarehouseDefault string
	// V01Principals 生产白名单身份：CSV "userID:ROLE,userID:ROLE"（ROL-01；空=演示模式 RoleOf）
	V01Principals string
	// AuthStrict 生产严格身份（B2B_AUTH_STRICT=true）：withContext 只认 X-WeCom-Context，
	// 无令牌/校验失败一律 401；false=演示/测试模式（保留 X-User-Id 信任，业务流转不变）。
	AuthStrict bool

	// ── 影子轨 S（docs/v0.1/12-shadow-full-flow.html；仅内部，零客户触达）──
	// ShadowMode=true 时启动守卫强制六项隔离（internal/shadowguard，A-01）。
	ShadowMode bool
	// TaskQueue Temporal task queue（A-03：A 轨 b2b-order / 影子轨 b2b-shadow-order）。
	TaskQueue string
	// ShadowPrincipals 影子 API 内部白名单：CSV "userID:ROLE,..."（S-铁1）。
	ShadowPrincipals string
	// ShadowIngestConfig 影子摄入源配置（结构复用 internal/sync.Config）。
	ShadowIngestConfig string
	// ShadowFeedConfig 事件 → Signal 映射配置（internal/shadow.FeedConfig）。
	ShadowFeedConfig string
	// ShadowReportDir 影子审计报告输出目录。
	ShadowReportDir string
}

// Load 从环境变量加载配置（带默认值）。
func Load() Config {
	return Config{
		HTTPAddr:                 getenv("B2B_HTTP_ADDR", ":8080"),
		PGDSN:                    getenv("B2B_PG_DSN", "postgres://postgres:postgres@localhost:55432/b2b_pilot_test?sslmode=disable"),
		PGAPIBaseURL:             getenv("B2B_PGAPI_BASE_URL", "https://api.renheng.com/pgapi"),
		PGAPIAPIKey:              getenv("B2B_PGAPI_API_KEY", ""),
		PGAPITimeout:             getenvDuration("B2B_PGAPI_TIMEOUT", 10*time.Second),
		PGAPIArchiveEnabled:      getenvBool("B2B_PGAPI_ARCHIVE_ENABLED", false),
		SpiceDBAddr:              getenv("B2B_SPICEDB_ADDR", "127.0.0.1:50051"),
		SpiceDBKey:               getenv("B2B_SPICEDB_KEY", "b2b-dev-key"),
		TemporalAddr:             getenv("B2B_TEMPORAL_ADDR", "127.0.0.1:7233"),
		TemporalNS:               getenv("B2B_TEMPORAL_NS", "default"),
		WeComProvider:            getenv("B2B_WECOM_PROVIDER", "mock"),
		WeComTenantCfg:           getenv("B2B_WECOM_TENANT_CFG", ""),
		WeComContextSecret:       getenv("B2B_WECOM_CONTEXT_SECRET", ""),
		AgentEnabled:             getenvBool("B2B_AGENT_ENABLED", false),
		AgentParserProvider:      getenv("B2B_AGENT_PARSER_PROVIDER", "mock"),
		AgentPromptVersion:       getenv("B2B_AGENT_PROMPT_VERSION", "v1"),
		AgentLLMBaseURL:          getenv("B2B_AGENT_LLM_BASE_URL", "http://121.40.34.96:18097/v1"),
		AgentLLMAPIKey:           getenv("B2B_AGENT_LLM_API_KEY", ""),
		AgentLLMModel:            getenv("B2B_AGENT_LLM_MODEL", "gpt-5.6"),
		AgentLLMTimeout:          getenvDuration("B2B_AGENT_LLM_TIMEOUT", 30*time.Second),
		WeComIngressEnabled:      getenvBool("B2B_WECOM_INGRESS_ENABLED", false),
		WeComIngressMaxRetry:     getenvInt("B2B_WECOM_INGRESS_MAX_RETRY", 5),
		WeComIngressCheckpoint:   getenv("B2B_WECOM_INGRESS_CHECKPOINT", "wecom_archive_cursor"),
		FeishuBridgeBaseURL:      getenv("B2B_FEISHU_BRIDGE_BASE_URL", ""),
		FeishuBridgeAppID:        getenv("B2B_FEISHU_BRIDGE_APP_ID", ""),
		FeishuBridgeRedirectURI:  getenv("B2B_FEISHU_BRIDGE_REDIRECT_URI", ""),
		FeishuBridgeEnabled:      getenvBool("B2B_FEISHU_BRIDGE_ENABLED", false),
		WeComAPIAddr:             getenv("B2B_API_ADDR", "http://127.0.0.1:18080"),
		ReceiptDays:              5,
		RemindHours:              24,
		EscalateHours:            48,
		AssigneeSupervisor:       getenv("B2B_ASSIGNEE_SUPERVISOR", "u-supervisor"),
		AssigneeChairman:         getenv("B2B_ASSIGNEE_CHAIRMAN", "u-chairman"),
		AssigneeOP:               getenv("B2B_ASSIGNEE_OP", "u-op"),
		AssigneeAccounting:       getenv("B2B_ASSIGNEE_ACCOUNTING", "u-accounting"),
		AssigneeSales:            getenv("B2B_ASSIGNEE_SALES", "u-sales"),
		AssigneeWarehousePrefix:  getenv("B2B_ASSIGNEE_WAREHOUSE_PREFIX", "u-wh-"),
		AssigneeWarehouseDefault: getenv("B2B_ASSIGNEE_WAREHOUSE_DEFAULT", "u-wh-yangyuan"),
		V01Principals:            getenv("B2B_V01_PRINCIPALS", ""),
		AuthStrict:               getenvBool("B2B_AUTH_STRICT", false),
		ShadowMode:               getenvBool("B2B_SHADOW_MODE", false),
		TaskQueue:                getenv("B2B_TASK_QUEUE", "b2b-order"),
		ShadowPrincipals:         getenv("B2B_SHADOW_PRINCIPALS", ""),
		ShadowIngestConfig:       getenv("SHADOW_INGEST_CONFIG", "config/shadow-ingest.example.json"),
		ShadowFeedConfig:         getenv("SHADOW_FEED_CONFIG", "config/shadow-feed.example.json"),
		ShadowReportDir:          getenv("SHADOW_REPORT_DIR", "logs/audit/shadow"),
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return def
}

func getenvBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func getenvInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}
