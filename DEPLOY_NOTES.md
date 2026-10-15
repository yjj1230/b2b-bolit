# pilot_v3 部署说明（121.40.226.192 · /opt/pilot_v3）

- 来源: https://git.renheng.com/carh-vesion/pilots/pilot_v3.git @ 85eb85d（2026-08-19, main HEAD）
- 部署日期: 2026-08-20
- 企微模式: **official 真实模式**（agent 1000007，corp wwe220eabd53e19a26，
  回调 token/aes_key 取自 /home/admin/b2b_pilot_v2/.env 同款 agent，见 config/wecom-tenant.json，权限 600）
- pgAPI key: 占位值（.env 中 B2B_PGAPI_API_KEY），需要真实数据时替换后
  `docker compose up -d --force-recreate worker`

## 端口（docker-compose.override.yml）
- postgres 宿主机 **25432**（原 15432 被 pilot-db-tunnel SSH 隧道占用）
- api 宿主机 **28080**（原 18080 被 enterprise-ai-assistant systemd 服务占用）
- wecom 宿主机 18081 / spicedb 15051 / temporal 17233 不变

## 上游 compose 修复（override 中，均标注原因）
1. spicedb serve 补 --grpc-preshared-key（默认 b2b-dev-key，与服务端 config.go 默认一致）
2. temporal 去掉不存在的 DYNAMIC_CONFIG_FILE_PATH
3. spicedb 镜像 v1.39.1 → v1.56.0（旧版无 healthcheck 子命令；镜像为 distroless，健康检查改为禁用）
4. spicedb-schema 服务重定义为 seedrel（tools 镜像）：写 schema.zed + 基线授权关系，幂等

## 外部路由（kbqa-public-gateway，server.js，备份 .bak-pilot-v3-20260820）
- https://wecom-test.renheng.com/wecom/callback/*  → 127.0.0.1:18081（v3 wecom 回调）
- https://wecom-test.renheng.com/wecom/healthz     → 127.0.0.1:18081
- 其余路径（/wecom/workbench、/api/v1/* 等）仍走 order-test/v2 BFF，未受影响

## 企微后台待办（人工）
- agent 1000007 回调 URL 需指向: **https://wecom-test.renheng.com/wecom/callback/chang-an**
  （注意：一个应用只有一个回调 URL，切换后原回调接收方将不再收到推送）
- 应用主页 /wecom/workbench 仍指向 order-test 工作台；如需改为 v3，v3 的 UI 在 api 服务
  （/assistant、/bridge），但该域名的根路径与 /api/v1/* 已被 order-test/v2 占用，需另行规划

## 数据
- PG 已播种演示世界（8 用户/2 订单/4 仓，run=deploy-20260820）；用户 wecom_user_id 为占位
  值（wecom_sales 等），真实企微用户登录前需 UPDATE users SET wecom_user_id='真实企微userid'
- SpiceDB 为内存态：spicedb 容器重建后 `docker compose up -d` 会自动重跑 seedrel

## 常用命令
cd /opt/pilot_v3
docker compose ps
docker compose logs -f api
docker compose restart api worker wecom
docker compose down            # 停止（保留数据卷）
- 构建辅助镜像: docker build -f Dockerfile.tools -t pilot_v3-tools:latest .

## v0.1 UI 域名接入（2026-08-20 追加）
- https://wecom-test.renheng.com/wecom/assistant → v3 api(:28080)/assistant（销售助理 H5/PC）
- https://wecom-test.renheng.com/wecom/bridge    → v3 api(:28080)/bridge（下单桥接后台）
- https://wecom-test.renheng.com/wecom/api/v1/*  → v3 api(:28080)/api/v1/*（UI 页面 API）
- 网关对 UI 页面 HTML 做了体替换：'/api/ → '/wecom/api/（JSON 不动）；jsapi-config 已验证返回
  agent 1000007 真实签名（wx.config + wx.agentConfig 双签名就绪）
- v2 BFF 的 /api/v1/*（根路径）与 order-test /miniapp 等原有路由全部复验正常
- 企微侧边栏/应用主页如需指向 v3：https://wecom-test.renheng.com/wecom/assistant
