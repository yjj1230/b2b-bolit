package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	os.Unsetenv("B2B_HTTP_ADDR")
	os.Unsetenv("B2B_PG_DSN")
	os.Unsetenv("B2B_SPICEDB_ADDR")
	os.Unsetenv("B2B_SPICEDB_KEY")
	os.Unsetenv("B2B_TEMPORAL_ADDR")
	os.Unsetenv("B2B_WECOM_PROVIDER")
	os.Unsetenv("B2B_WECOM_TENANT_CFG")
	os.Unsetenv("B2B_WECOM_CONTEXT_SECRET")
	os.Unsetenv("B2B_PGAPI_BASE_URL")
	os.Unsetenv("B2B_PGAPI_API_KEY")
	os.Unsetenv("B2B_PGAPI_TIMEOUT")
	os.Unsetenv("B2B_PGAPI_ARCHIVE_ENABLED")
	os.Unsetenv("B2B_FEISHU_BRIDGE_BASE_URL")
	os.Unsetenv("B2B_FEISHU_BRIDGE_APP_ID")
	os.Unsetenv("B2B_FEISHU_BRIDGE_REDIRECT_URI")
	os.Unsetenv("B2B_FEISHU_BRIDGE_ENABLED")
	os.Unsetenv("B2B_AGENT_LLM_BASE_URL")
	os.Unsetenv("B2B_AGENT_LLM_API_KEY")
	os.Unsetenv("B2B_AGENT_LLM_MODEL")
	os.Unsetenv("B2B_AGENT_LLM_TIMEOUT")
	os.Unsetenv("B2B_SHADOW_MODE")
	os.Unsetenv("B2B_TASK_QUEUE")
	os.Unsetenv("B2B_SHADOW_PRINCIPALS")
	os.Unsetenv("SHADOW_INGEST_CONFIG")
	os.Unsetenv("SHADOW_FEED_CONFIG")
	os.Unsetenv("SHADOW_REPORT_DIR")
	c := Load()
	if c.HTTPAddr != ":8080" || c.SpiceDBKey != "b2b-dev-key" || c.WeComProvider != "mock" {
		t.Fatalf("defaults mismatch: %+v", c)
	}
	if c.WeComTenantCfg != "" || c.WeComContextSecret != "" {
		t.Fatalf("wecom injection defaults mismatch: %+v", c)
	}
	if c.PGAPIBaseURL != "https://api.renheng.com/pgapi" || c.PGAPITimeout == 0 || c.FeishuBridgeEnabled {
		t.Fatalf("pgapi defaults mismatch: %+v", c)
	}
	if c.PGAPIArchiveEnabled {
		t.Fatalf("pgapi archive should default off: %+v", c)
	}
	if c.ReceiptDays != 5 || c.RemindHours != 24 || c.EscalateHours != 48 {
		t.Fatalf("business rule defaults mismatch: %+v", c)
	}
	if c.PGDSN == "" || c.SpiceDBAddr == "" || c.TemporalAddr == "" {
		t.Fatalf("empty infra defaults: %+v", c)
	}
	if c.ShadowMode || c.TaskQueue != "b2b-order" || c.ShadowPrincipals != "" {
		t.Fatalf("shadow defaults mismatch: %+v", c)
	}
	if c.AgentLLMBaseURL != "http://121.40.34.96:18097/v1" || c.AgentLLMModel != "gpt-5.6" ||
		c.AgentLLMTimeout != 30*time.Second || c.AgentLLMAPIKey != "" {
		t.Fatalf("agent llm defaults mismatch: %+v", c)
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	os.Setenv("B2B_HTTP_ADDR", ":9999")
	os.Setenv("B2B_PG_DSN", "postgres://x:y@z:1/db")
	os.Setenv("B2B_SPICEDB_ADDR", "1.2.3.4:50051")
	os.Setenv("B2B_SPICEDB_KEY", "secret-key")
	os.Setenv("B2B_TEMPORAL_ADDR", "temporal.example:7233")
	os.Setenv("B2B_TEMPORAL_NS", "b2b-ns")
	os.Setenv("B2B_WECOM_PROVIDER", "official")
	os.Setenv("B2B_WECOM_TENANT_CFG", "/etc/wecom/tenant.json")
	os.Setenv("B2B_WECOM_CONTEXT_SECRET", "prod-context-secret")
	os.Setenv("B2B_PGAPI_BASE_URL", "http://pgapi.test/pgapi")
	os.Setenv("B2B_PGAPI_API_KEY", "test-key")
	os.Setenv("B2B_PGAPI_TIMEOUT", "3")
	os.Setenv("B2B_PGAPI_ARCHIVE_ENABLED", "true")
	os.Setenv("B2B_FEISHU_BRIDGE_BASE_URL", "https://old.example")
	os.Setenv("B2B_FEISHU_BRIDGE_APP_ID", "cli_test")
	os.Setenv("B2B_FEISHU_BRIDGE_REDIRECT_URI", "https://v01.example/bridge/callback")
	os.Setenv("B2B_FEISHU_BRIDGE_ENABLED", "true")
	os.Setenv("B2B_AGENT_LLM_BASE_URL", "http://llm.test/v1")
	os.Setenv("B2B_AGENT_LLM_API_KEY", "test-llm-key")
	os.Setenv("B2B_AGENT_LLM_MODEL", "gpt-test")
	os.Setenv("B2B_AGENT_LLM_TIMEOUT", "7")
	defer os.Unsetenv("B2B_HTTP_ADDR")
	defer os.Unsetenv("B2B_PG_DSN")
	defer os.Unsetenv("B2B_SPICEDB_ADDR")
	defer os.Unsetenv("B2B_SPICEDB_KEY")
	defer os.Unsetenv("B2B_TEMPORAL_ADDR")
	defer os.Unsetenv("B2B_TEMPORAL_NS")
	defer os.Unsetenv("B2B_WECOM_PROVIDER")
	defer os.Unsetenv("B2B_WECOM_TENANT_CFG")
	defer os.Unsetenv("B2B_WECOM_CONTEXT_SECRET")
	defer os.Unsetenv("B2B_PGAPI_BASE_URL")
	defer os.Unsetenv("B2B_PGAPI_API_KEY")
	defer os.Unsetenv("B2B_PGAPI_TIMEOUT")
	defer os.Unsetenv("B2B_PGAPI_ARCHIVE_ENABLED")
	defer os.Unsetenv("B2B_FEISHU_BRIDGE_BASE_URL")
	defer os.Unsetenv("B2B_FEISHU_BRIDGE_APP_ID")
	defer os.Unsetenv("B2B_FEISHU_BRIDGE_REDIRECT_URI")
	defer os.Unsetenv("B2B_FEISHU_BRIDGE_ENABLED")
	defer os.Unsetenv("B2B_AGENT_LLM_BASE_URL")
	defer os.Unsetenv("B2B_AGENT_LLM_API_KEY")
	defer os.Unsetenv("B2B_AGENT_LLM_MODEL")
	defer os.Unsetenv("B2B_AGENT_LLM_TIMEOUT")

	c := Load()
	if c.HTTPAddr != ":9999" || c.PGDSN != "postgres://x:y@z:1/db" || c.SpiceDBAddr != "1.2.3.4:50051" ||
		c.SpiceDBKey != "secret-key" || c.TemporalAddr != "temporal.example:7233" ||
		c.TemporalNS != "b2b-ns" || c.WeComProvider != "official" || c.WeComTenantCfg != "/etc/wecom/tenant.json" ||
		c.WeComContextSecret != "prod-context-secret" {
		t.Fatalf("env override mismatch: %+v", c)
	}
	if c.PGAPIBaseURL != "http://pgapi.test/pgapi" || c.PGAPIAPIKey != "test-key" || c.PGAPITimeout != 3*time.Second {
		t.Fatalf("pgapi env override mismatch: %+v", c)
	}
	if !c.PGAPIArchiveEnabled {
		t.Fatalf("pgapi archive env override mismatch: %+v", c)
	}
	if c.FeishuBridgeBaseURL != "https://old.example" || c.FeishuBridgeAppID != "cli_test" ||
		c.FeishuBridgeRedirectURI != "https://v01.example/bridge/callback" || !c.FeishuBridgeEnabled {
		t.Fatalf("feishu bridge env override mismatch: %+v", c)
	}
	if c.AgentLLMBaseURL != "http://llm.test/v1" || c.AgentLLMAPIKey != "test-llm-key" ||
		c.AgentLLMModel != "gpt-test" || c.AgentLLMTimeout != 7*time.Second {
		t.Fatalf("agent llm env override mismatch: %+v", c)
	}
}
