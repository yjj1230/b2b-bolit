package shadowguard

import (
	"strings"
	"testing"

	"b2b_pilot_v3/internal/config"
)

func validConfig() config.Config {
	return config.Config{
		ShadowMode:          true,
		PGDSN:               "postgres://postgres:postgres@shadow-postgres:5432/b2b_shadow?sslmode=disable",
		SpiceDBAddr:         "shadow-spicedb:50051",
		TemporalNS:          ShadowNamespace,
		TaskQueue:           ShadowTaskQueue,
		WeComProvider:       "mock",
		FeishuBridgeEnabled: false,
	}
}

func TestGuardAcceptsValidShadowConfig(t *testing.T) {
	if err := Validate(validConfig()); err != nil {
		t.Fatalf("valid shadow config rejected: %v", err)
	}
}

// S-07 负向守卫：把生产 DSN / 生产队列 / default namespace / official 企微 /
// bridge=true / 生产 SpiceDB 依次喂给影子二进制，全部拒绝启动（A-01 六项）。
func TestGuardNegativeMatrix(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{"production DSN", func(c *config.Config) {
			c.PGDSN = "postgres://postgres:postgres@postgres:5432/b2b_pilot?sslmode=disable"
		}, ForbiddenDBToken},
		{"non-shadow database", func(c *config.Config) {
			c.PGDSN = "postgres://postgres:postgres@shadow-postgres:5432/postgres?sslmode=disable"
		}, ShadowDBName},
		{"default namespace", func(c *config.Config) { c.TemporalNS = "default" }, ShadowNamespace},
		{"production queue", func(c *config.Config) { c.TaskQueue = "b2b-order" }, ShadowTaskQueue},
		{"official wecom", func(c *config.Config) { c.WeComProvider = "official" }, "mock"},
		{"bridge enabled", func(c *config.Config) { c.FeishuBridgeEnabled = true }, "B2B_FEISHU_BRIDGE_ENABLED"},
		{"production spicedb", func(c *config.Config) { c.SpiceDBAddr = "spicedb:50051" }, "allowlist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg)
			err := Validate(cfg)
			if err == nil {
				t.Fatalf("expected rejection, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestGuardRequiresExplicitShadowOptIn(t *testing.T) {
	cfg := validConfig()
	cfg.ShadowMode = false
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "B2B_SHADOW_MODE") {
		t.Fatalf("expected shadow-mode opt-in error, got %v", err)
	}
}

func TestGuardAcceptsLoopbackSpiceDBForLocalReplay(t *testing.T) {
	cfg := validConfig()
	cfg.SpiceDBAddr = "127.0.0.1:15052"
	if err := Validate(cfg); err != nil {
		t.Fatalf("loopback shadow spicedb rejected: %v", err)
	}
}

func TestValidatePrincipals(t *testing.T) {
	if err := ValidatePrincipals("dev-a:ADMIN,audit-b:AUDITOR"); err != nil {
		t.Fatalf("valid principals rejected: %v", err)
	}
	if err := ValidatePrincipals(""); err == nil {
		t.Fatal("empty principals accepted")
	}
	if err := ValidatePrincipals("dev-a"); err == nil {
		t.Fatal("malformed principals accepted")
	}
}
