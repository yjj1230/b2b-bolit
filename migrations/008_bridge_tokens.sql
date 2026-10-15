-- 008_bridge_tokens.sql —— 桥接飞书授权 token 持久化缓存
-- 目的：token 存 PG，容器重建后仍可免重复授权（TTL 内）；
--       身份在浏览器 localStorage，token 在服务端，二者生命周期解耦。
-- 幂等原则：IF NOT EXISTS，可重复执行。

BEGIN;

CREATE TABLE IF NOT EXISTS bridge_tokens (
    user_id    TEXT PRIMARY KEY,
    token      TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
