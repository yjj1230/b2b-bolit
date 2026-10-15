// cmd/api —— HTTP API 服务（docs/02-architecture.html §2.1）
// 运行模式：
//
//	B2B_TEMPORAL_ADDR 设置 → Temporal 客户端模式（生产/演示全栈）
//	未设置            → LocalRunner 内嵌模式（HTTP API + 企微 mock，无需 Temporal）
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"go.temporal.io/sdk/client"

	"b2b_pilot_v3/internal/api"
	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/bridge"
	"b2b_pilot_v3/internal/config"
	"b2b_pilot_v3/internal/integration"
	"b2b_pilot_v3/internal/orderdata"
	"b2b_pilot_v3/internal/repo"
	"b2b_pilot_v3/internal/shadowguard"
	"b2b_pilot_v3/internal/wecom"
	"b2b_pilot_v3/internal/workflows"
)

func main() {
	cfg := config.Load()
	ctx := context.Background()

	// SH-13/A-01：影子模式启动守卫（六项 fail-fast）+ 身份白名单强制（S-铁1）。
	if cfg.ShadowMode {
		if err := shadowguard.Validate(cfg); err != nil {
			log.Fatalf("shadow guard: %v", err)
		}
		if err := shadowguard.ValidatePrincipals(cfg.ShadowPrincipals); err != nil {
			log.Fatalf("shadow principals: %v", err)
		}
	}

	store, err := repo.New(ctx, cfg.PGDSN)
	if err != nil {
		log.Fatalf("connect pg: %v", err)
	}
	defer store.Close()

	// 企微出站通道（wrapper 隔离点：mock | official | thirdparty，07 §6）
	// official 配置经 B2B_WECOM_TENANT_CFG 注入：测试环境与生产环境分别挂载各自 JSON。
	gw, err := wecom.NewGateway(cfg)
	if err != nil {
		log.Fatalf("wecom gateway: %v", err)
	}
	notifier := wecom.NewNotifier(gw)
	var groupSender wecom.GroupSender
	if gs, ok := gw.(wecom.GroupSender); ok {
		// 守门员确认后的客户群出站：mock 记录 / official appchat 发送
		groupSender = gs
	}

	// Starter + 授权判定：
	//  - Temporal（生产）模式：Temporal 客户端 + SpiceDB（关系授权；Outbox 投影器
	//    阶段二接入后关系自动写入）
	//  - 本地内嵌模式：LocalRunner + StaticChecker 对照表替身（L0-09；与 tests/ E2E
	//    同一判定路径——本地模式是「HTTP API + 企微 mock 测试模式」）
	var starter api.Starter
	var checker authz.Checker
	if os.Getenv("B2B_TEMPORAL_ADDR") != "" {
		c, err := client.Dial(client.Options{
			HostPort:  cfg.TemporalAddr,
			Namespace: cfg.TemporalNS,
		})
		if err != nil {
			log.Fatalf("dial temporal: %v", err)
		}
		defer c.Close()
		ts := workflows.NewTemporalStarter(c, store)
		if cfg.ShadowMode {
			// SH-14/A-03：影子队列 + shadow- WorkflowID 前缀（与 A 轨 ID 空间完全隔离）
			ts.ConfigureQueue(cfg.TaskQueue, workflows.ShadowWorkflowIDPrefix)
		} else {
			ts.ConfigureQueue(cfg.TaskQueue, "")
		}
		starter = ts

		sc, err := authz.NewSpiceDB(cfg.SpiceDBAddr, cfg.SpiceDBKey)
		if err != nil {
			log.Fatalf("connect spicedb: %v", err)
		}
		checker = sc
		log.Printf("mode=temporal addr=%s", cfg.TemporalAddr)
	} else {
		// 本地演示模式：虚拟时长策略（integration.NewDemoPolicy，10s），
		// 避免内嵌 TestSuite 自动推进烧穿运行预算；生产时长见 StaticConfigProvider。
		// 订单数据实体：配置了 pgAPI Key 则走真实 pgAPI，否则使用 demo Provider。
		checker = authz.NewStaticChecker(demoRoleResolver)
		var data orderdata.Provider
		if cfg.PGAPIAPIKey != "" {
			hp, err := orderdata.NewHTTPProviderWithTimeout(cfg.PGAPIBaseURL, cfg.PGAPIAPIKey, cfg.PGAPITimeout)
			if err != nil {
				log.Fatalf("orderdata pgapi: %v", err)
			}
			data = hp
		} else {
			data = orderdata.NewDemoMemoryProvider()
		}
		lr := api.NewLocalRunnerWithPolicyAndData(store, data, notifier, checker, integration.NewDemoPolicy())
		lr.SetLogger(log.Printf)
		starter = lr
		log.Printf("mode=local (embedded workflows, no Temporal; demo policy %v)", integration.DemoDuration)
	}

	roleOf := demoRoleOf
	srv := api.NewServer(store, starter, checker, roleOf)
	srv.LocalSalesPersons = store
	// INV-01：税号校验需要订单数据实体 Provider（pgAPI key 配置时启用；未配置跳过比对）
	if cfg.PGAPIAPIKey != "" {
		hp, err := orderdata.NewHTTPProviderWithTimeout(cfg.PGAPIBaseURL, cfg.PGAPIAPIKey, cfg.PGAPITimeout)
		if err != nil {
			log.Fatalf("orderdata pgapi: %v", err)
		}
		srv.Data = hp
	}
	srv.PGAPIArchiveEnabled = cfg.PGAPIArchiveEnabled
	srv.AuthStrict = cfg.AuthStrict
	// ROL-01 收敛：审批人路由配置化（B2B_ASSIGNEE_*），生产/演示统一注入。
	assigneeCfg := workflows.AssigneeConfig{
		Supervisor: cfg.AssigneeSupervisor, Chairman: cfg.AssigneeChairman,
		OP: cfg.AssigneeOP, Accounting: cfg.AssigneeAccounting, Sales: cfg.AssigneeSales,
		WarehousePrefix: cfg.AssigneeWarehousePrefix, WarehouseFallback: cfg.AssigneeWarehouseDefault,
	}
	switch st := starter.(type) {
	case *workflows.TemporalStarter:
		st.SetAssignees(assigneeCfg)
	case *api.LocalRunner:
		st.SetAssignees(assigneeCfg)
	}
	// ROL-01 收敛：生产白名单身份（B2B_V01_PRINCIPALS=userID:ROLE,...）；未配置保持演示语义。
	// 影子 API：身份白名单强制取 B2B_SHADOW_PRINCIPALS（S-铁1，启动已校验非空）。
	principalsRaw := cfg.V01Principals
	if cfg.ShadowMode {
		principalsRaw = cfg.ShadowPrincipals
	}
	if principalsRaw != "" {
		principals := parsePrincipals(principalsRaw)
		srv.RoleResolver = func(userID string) (authz.Role, string, bool) {
			role, ok := principals[userID]
			tenant := "chang-an"
			if cfg.ShadowMode {
				tenant = shadowguard.ShadowTenant // A-03：影子租户后缀，与 A 轨租户空间隔离
			}
			return role, tenant, ok
		}
		log.Printf("strict principals=%d", len(principals))
	}
	// S1 侧边栏 H5 服务端能力（07 §6.1；对齐 v2 bff-api）：JS-SDK 双签名 + OAuth 交换 + 上下文令牌。
	// mock：确定性签名/内置 code 表；official：corp/agent/secret/token/AESKey 全部来自
	// B2B_WECOM_TENANT_CFG 注入的租户 JSON（测试/生产环境分别挂载）。
	srv.GroupSender = groupSender
	srv.Context = wecom.NewContextIssuer(cfg.WeComContextSecret, 2*time.Hour)
	if ogw, ok := gw.(*wecom.OfficialGateway); ok {
		jsapi, err := wecom.NewOfficialJSAPIConfigBuilder(ogw)
		if err != nil {
			log.Fatalf("official jsapi: %v", err)
		}
		srv.JSAPI = jsapi
		srv.Identity = wecom.NewOfficialIdentityProvider(ogw, store)
	} else {
		srv.JSAPI = wecom.NewJSAPIConfigBuilder("ww-chang-an-demo", "1000002", "b2b-demo-jsapi-secret")
		srv.Identity = wecom.NewMockIdentityProvider("chang-an", map[string]string{
			"mock-supervisor": "u-supervisor",
			"mock-warehouse":  "u-wh-yangyuan",
			"mock-accounting": "u-accounting",
			"mock-contact":    "u-contact",
		}, map[string]string{
			"chat-cust-0001": "cust-0001",
			"chat-cust-0002": "cust-0002",
		})
	}
	if cfg.FeishuBridgeEnabled {
		bc, err := bridge.New(bridge.Config{
			BaseURL:     cfg.FeishuBridgeBaseURL,
			AppID:       cfg.FeishuBridgeAppID,
			RedirectURI: cfg.FeishuBridgeRedirectURI,
			Recorder:    store,
			// token 持久化到 PG：容器重建后 TTL 内免重复授权（身份在浏览器 localStorage，token 在服务端）。
			Cache: bridgeTokenCache{store},
		})
		if err != nil {
			log.Fatalf("feishu bridge: %v", err)
		}
		srv.Bridge = bc
		srv.BridgeLedger = store
		log.Printf("feishu bridge enabled base=%s", cfg.FeishuBridgeBaseURL)
	}
	addr := cfg.HTTPAddr
	log.Printf("api listening on %s", addr)
	srvHTTP := &http.Server{Addr: addr, Handler: srv.Routes(), ReadHeaderTimeout: 10 * time.Second}
	if err := srvHTTP.ListenAndServe(); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

// demoRoleOf 演示模式角色解析（X-User-Id → 角色；正式鉴权走 SpiceDB + 身份服务）。
func demoRoleOf(userID string) (authz.Role, string) {
	switch userID {
	case "frank-wh", "u-wh-yangyuan", "u-wh-dongguan":
		return authz.RoleWarehouse, "chang-an"
	case "u-accounting", "erin-acct":
		return authz.RoleAccounting, "chang-an"
	case "u-chairman", "carol-chair":
		return authz.RoleChairman, "chang-an"
	case "u-supervisor", "bob-super":
		return authz.RoleSupervisor, "chang-an"
	case "u-admin", "grace-admin":
		return authz.RoleAdmin, "chang-an"
	case "u-contact":
		return authz.RoleContact, "chang-an" // 客户联系人：BFF 投影剥离价格/金额（场景 D）
	}
	return authz.RoleOP, "chang-an"
}

// parsePrincipals 解析 B2B_V01_PRINCIPALS（userID:ROLE 逗号分隔）。
func parsePrincipals(raw string) map[string]authz.Role {
	out := map[string]authz.Role{}
	for _, part := range strings.Split(raw, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), ":", 2)
		if len(kv) != 2 || kv[0] == "" {
			continue
		}
		out[kv[0]] = authz.Role(kv[1])
	}
	return out
}

// demoRoleResolver 演示模式角色解析（与 demoRoleOf 对齐；StaticChecker 对照表用）。
func demoRoleResolver(userID string) (authz.Role, error) {
	switch userID {
	case "frank-wh", "u-wh-yangyuan", "u-wh-dongguan":
		return authz.RoleWarehouse, nil
	case "u-accounting", "erin-acct":
		return authz.RoleAccounting, nil
	case "u-chairman", "carol-chair":
		return authz.RoleChairman, nil
	case "u-supervisor", "bob-super":
		return authz.RoleSupervisor, nil
	case "u-admin", "grace-admin":
		return authz.RoleAdmin, nil
	case "u-sales", "dave-op", "u-op", "bob-op":
		return authz.RoleOP, nil
	case "u-contact":
		return authz.RoleContact, nil
	}
	return authz.RoleOP, nil
}

// bridgeTokenCache 把 repo 的 PG token 存储适配为 bridge.TokenCache（跨容器重启持久）。
type bridgeTokenCache struct{ store *repo.Repo }

func (c bridgeTokenCache) Get(userID string, now time.Time) (string, time.Time, bool) {
	return c.store.GetBridgeToken(context.Background(), userID, now)
}

func (c bridgeTokenCache) Put(userID, token string, expiresAt time.Time) error {
	return c.store.PutBridgeToken(context.Background(), userID, token, expiresAt)
}
