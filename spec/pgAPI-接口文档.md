# pgAPI 接口文档（公网版）

> 版本：v1.1 | 更新日期：2026-08-17
> 适用：企微应用 / 外部系统对接产品池、客户池、购销合同数据

---

## 1. 服务信息

| 项目 | 值 |
|-|-|
| 服务名称 | pgAPI（PostgreSQL 业务数据接口） |
| 服务地址 | `https://api.renheng.com/pgapi` |
| 数据格式 | JSON |
| 鉴权方式 | Header 携带 API Key |
| API Key | `pgapi_1da296aaed0e574ae0f9b2a1d805a3b4ab542a89` |

**所有请求必须携带请求头：**
```
X-API-Key: pgapi_1da296aaed0e574ae0f9b2a1d805a3b4ab542a89
```

**免鉴权接口（健康检查）：**
- `GET https://api.renheng.com/pgapi/health`
- `GET https://api.renheng.com/pgapi/health/db`

> 业务接口未携带 Key 或 Key 错误时返回 `401 {"detail":"Invalid or missing API key"}`。

---

## 2. 通用响应格式

### 2.1 成功响应（单条 / 操作类）
```json
{
  "success": true,
  "data": { ... },
  "message": "ok"
}
```

### 2.2 成功响应（列表类）
```json
{
  "success": true,
  "data": {
    "items": [ ... ],
    "page": 1,
    "page_size": 20,
    "total": 302,
    "pagination": {
      "page": 1,
      "page_size": 20,
      "total": 302,
      "has_next": true
    },
    "sort": {
      "sort_by": "_last_modified_time",
      "sort_order": "desc"
    }
  },
  "message": "ok"
}
```
> 业务开发主要读取：`data.items`（当前页数据）、`data.total`（总数）、`data.pagination.has_next`（是否还有下一页）。

### 2.3 失败响应
```json
{
  "success": false,
  "error_code": "DATABASE_ERROR",
  "message": "数据库查询失败",
  "request_id": "..."
}
```

### 2.4 必填字段校验失败
```json
{
  "detail": "Missing required field(s): customer_name, customer_level"
}
```

---

## 3. 通用查询参数（所有列表接口）

| 参数 | 类型 | 必填 | 默认 | 说明 |
|-|-|-|-|-|
| `page` | integer | 否 | 1 | 页码，从 1 开始 |
| `page_size` | integer | 否 | 20 | 每页数量，**最大 200** |
| `q` | string | 否 | 无 | 关键词搜索 |
| `keyword` | string | 否 | 无 | 与 `q` 等价 |
| `sort_by` | string | 否 | 各资源默认字段 | 按 API 字段排序 |
| `sort_order` | string | 否 | desc | `asc` 或 `desc` |
| `include_deleted` | boolean | 否 | false | 是否包含软删除记录 |
| 任意字段名 | string | 否 | 无 | 按已暴露字段模糊过滤，如 `customer_name=科技` |

**注意**：中文参数需 URL 编码（如 `q=科技` → `q=%E7%A7%91%E6%8A%80`），或使用 `--data-urlencode`。

---

## 4. 产品池接口（products）

产品池用于下单时查询产品、规格、分类等基础资料。

### 4.1 查询产品列表

```
GET /api/db/products/search
GET /api/db/products        （等价别名）
```

**常用查询：**
```
GET /api/db/products/search?page=1&page_size=10
GET /api/db/products/search?q=BT&page=1&page_size=10
GET /api/db/products/search?product_name=BT
GET /api/db/products/search?category_large=化工
GET /api/db/products/search?spec=25kg
```

**响应示例：**
```json
{
  "success": true,
  "data": {
    "items": [
      {
        "record_id": "recuIuswMsvscA",
        "product_name": "BTD-28",
        "spec": "BTD-28",
        "tech_spec": "BT-38",
        "category_major": "造纸化学品",
        "category_minor": "造纸化学品",
        "major_type": "造纸化学品",
        "minor_type": "膨润土",
        "is_mortar": "否"
      }
    ],
    "page": 1,
    "page_size": 10,
    "total": 154
  },
  "message": "ok"
}
```

**产品池字段说明：**

| 字段名 | 说明 | 关键词搜索 | 新增必填 |
|-|-|-|-|
| `product_name` | 产品名称 | ✅ | ✅ |
| `spec` | 规格（如 25kg） | ✅ | ✅ |
| `tech_spec` | 规格-推荐 | ✅ | 否 |
| `category_major` | 产品分类大类（同 `business_category_large`） | ✅ | 否 |
| `category_minor` | 产品分类小类（同 `business_category_small`） | ✅ | 否 |
| `major_type` | 大类（同 `category_large`） | — | 否 |
| `minor_type` | 小类（同 `category_small`） | — | 否 |
| `is_mortar` | 是否为砂浆 | — | 否 |

### 4.2 查询单个产品

```
GET /api/db/products/{record_id}
```

**示例：** `GET /api/db/products/recuIuswMsvscA`

### 4.3 新增产品

```
POST /api/db/products
Content-Type: application/json
```

**请求体示例：**
```json
{
  "product_name": "BT-38",
  "spec": "25kg",
  "tech_spec": "技术规格说明",
  "category_major": "造纸化学品",
  "category_minor": "助剂",
  "is_mortar": false
}
```

**说明：**
- 必填字段：`product_name`、`spec`
- `record_id` 可不传，服务端自动生成 `api_` 前缀 ID
- 返回 201 及完整记录

### 4.4 修改产品

```
PATCH /api/db/products/{record_id}
Content-Type: application/json
```

**请求体示例：**
```json
{
  "spec": "20kg",
  "category_minor": "新分类"
}
```

### 4.5 删除产品

```
DELETE /api/db/products/{record_id}
```
> ⚠️ 当前产品池没有统一停用字段，删除接口返回 400「不支持业务删除」。

---

## 5. 客户池接口（customers）

客户池用于下单时查询客户、税号、地址、收件人等信息。

### 5.1 查询客户列表

```
GET /api/db/customers/search
GET /api/db/customers        （等价别名）
```

**常用查询：**
```
GET /api/db/customers/search?page=1&page_size=10
GET /api/db/customers/search?q=科技
GET /api/db/customers/search?customer_name=科技
GET /api/db/customers/search?tax_no=913
GET /api/db/customers/search?phone=138
```

**响应示例：**
```json
{
  "success": true,
  "data": {
    "items": [
      {
        "record_id": "rec_xxx",
        "customer_no": "CUST-001",
        "customer_name": "某某科技有限公司",
        "supplier": "浙江长安仁恒科技股份有限公司",
        "sales_users": [{"id": "Huan.", "name": "马弘毅"}],
        "customer_level": "A",
        "customer_type": "企业客户",
        "tax_no": "913xxxxxxxxxxxxxxx",
        "invoice_tax_no": "913xxxxxxxxxxxxxxx",
        "phone": "13800000000",
        "shipping_address": "上海市...",
        "receiver": "张三",
        "payment_terms": "款到发货"
      }
    ],
    "page": 1,
    "page_size": 10,
    "total": 3022
  },
  "message": "ok"
}
```

**客户池字段说明：**

| 字段名 | 说明 | 关键词搜索 | 新增必填 |
|-|-|-|-|
| `customer_name` | 客户名称 | ✅ | ✅ |
| `customer_level` | 客户等级 | — | ✅ |
| `customer_type` | 客户类型 | — | ✅ |
| `supplier` | 供方 | — | ✅ |
| `customer_no` | 客户编号 | ✅ | 否 |
| `tax_no` | 税号 | ✅ | 否 |
| `invoice_tax_no` | 开票税号 | ✅ | 否 |
| `phone` | 电话 | ✅ | 否 |
| `sales_users` | 销售人员（对象数组） | — | 否 |
| `salesperson` | 销售人员（同 `sales_users`） | — | 否 |
| `department` | 部门 | — | 否 |
| `customer_nature` | 客户性质 | — | 否 |
| `relation_status` | 关联状态 | — | 否 |
| `shipping_address` | 收货地址 | — | 否 |
| `receiver` | 收件人 | — | 否 |
| `registered_address` | 注册地址 | — | 否 |
| `bank_name` | 开户行 | — | 否 |
| `bank_account` | 银行账号 | — | 否 |
| `payment_terms` | 账期/结算方式 | — | 否 |

> ⚠️ `sales_users` 是人员对象数组，人员姓名读取 `sales_users[0].name`；多销售员时数组中多个对象。

### 5.2 查询单个客户

```
GET /api/db/customers/{record_id}
```

### 5.3 查询客户地址

```
GET /api/db/customers/{record_id}/addresses
```

**响应示例：**
```json
{
  "success": true,
  "data": {
    "record_id": "rec_xxx",
    "customer_name": "某某科技有限公司",
    "receiver": "张三",
    "phone": "13800000000",
    "shipping_address": "上海市...",
    "registered_address": "上海市..."
  },
  "message": "ok"
}
```

### 5.4 新增客户

```
POST /api/db/customers
Content-Type: application/json
```

**请求体示例：**
```json
{
  "customer_name": "某某科技有限公司",
  "customer_level": "A",
  "customer_type": "企业客户",
  "supplier": "浙江长安仁恒科技股份有限公司",
  "customer_no": "CUST-001",
  "tax_no": "913xxxxxxxxxxxxxxx",
  "invoice_tax_no": "913xxxxxxxxxxxxxxx",
  "receiver": "张三",
  "phone": "13800000000",
  "shipping_address": "上海市...",
  "registered_address": "上海市...",
  "bank_name": "中国银行上海分行",
  "bank_account": "1234567890",
  "payment_terms": "款到发货"
}
```

**说明：**
- 必填字段：`customer_name`、`customer_level`、`customer_type`、`supplier`
- `record_id` 可不传，服务端自动生成
- 返回 201 及完整记录

### 5.5 修改客户

```
PATCH /api/db/customers/{record_id}
Content-Type: application/json
```

**请求体示例：**
```json
{
  "phone": "13900000000",
  "shipping_address": "新的收货地址"
}
```

### 5.6 删除客户

```
DELETE /api/db/customers/{record_id}
```
> ⚠️ 当前客户池没有统一停用字段，删除接口返回 400「不支持业务删除」。

---

## 6. 购销合同接口（purchase-contracts）

购销合同（A.购销合同登记表）是 RHCRM 统一订单数据源（PG `rhcrm.t_tbl1neg3iymvqdvi`）。
接口语义为 **查询 + 仅允许新增**（修改/删除受保护）。

**数据说明：**
- 数据来源：飞书原生（存量）+ 飞书三方审批（事件订阅同步）+ 未来企微订单（经 pgAPI 写入）
- 现场 PG 已同步 8,784 行（存量 7,485 + 三方审批 1,299）；飞书侧当前有效约 6,383 条
- **默认过滤**：查询自动排除「已撤回」「已拒绝」状态的记录（与飞书视图口径一致）
- b2b v0.1（D-007 Q1=B）仅使用查询；企微订单写路径下一迭代启用，
  状态回写采用 **apply_no upsert（D-007 Q2=U2）**：同 apply_no 再 POST 即更新白名单字段

### 6.1 查询合同列表

```
GET /api/db/purchase-contracts/search
GET /api/db/purchase-contracts        （等价别名）
```

**常用查询：**
```
# 分页查询全部
GET /api/db/purchase-contracts/search?page=1&page_size=20

# 按销售员查询（推荐用于订单查询）+ 分页
GET /api/db/purchase-contracts/search?sales_user=张有连&page=1&page_size=20

# 按客户名称查询
GET /api/db/purchase-contracts/search?customer_name=上海瑞琦

# 按申请编号 / 合同编号查询
GET /api/db/purchase-contracts/search?apply_no=202604020046
GET /api/db/purchase-contracts/search?contract_no=RH-20260402

# 组合查询（销售员 + 状态 + 分页）
GET /api/db/purchase-contracts/search?sales_user=张有连&order_status=已通过&page=1&page_size=20

# 关键词搜索（匹配 申请编号/客户名称/产品名称/合同编号/供方）
GET /api/db/purchase-contracts/search?q=上海瑞琦&page=1&page_size=20
```

**响应示例：**
```json
{
  "success": true,
  "data": {
    "items": [
      {
        "record_id": "recvfCE6ZVDLjK",
        "deleted": false,
        "created_time": null,
        "last_modified_time": null,
        "synced_at": "2026-08-17T08:28:12.766311+08:00",
        "apply_no": "202604020046",
        "customer_name": "上海瑞琦实业有限公司",
        "supplier": "浙江长安仁恒科技股份有限公司上海分公司",
        "product_name": "Clayminton 72",
        "spec": ["HD810"],
        "quantity_kg": [1000],
        "unit_price": 22.0,
        "tax_total_amount": 22000.0,
        "contract_no": "RH-20260402",
        "order_status": "已通过",
        "approval_node": null,
        "approval_flow": "购销合同订单申请",
        "ship_status": ["未发货"],
        "stock_status": null,
        "invoice_status": "是",
        "sales_user": "张有连",
        "remark": null,
        "source_id": "NzQ5MzcwODU1NzUxNzIyNjAwMzozMTlBNzdBMS1DN0Y5LTQ1RjUtOTA2RS0zRUY4NDJCMThCQzYtMjpmMzA0ZGUwYjFmNmQzYTNhNWUxM2YyMTlhNDZjNWU4OTox",
        "ai_contract_upload": [
          {
            "url": "https://open.feishu.cn/open-apis/drive/v1/medias/D4t6bUMbdon9yCxvlnDcIkAJnkd/download?extra=...",
            "name": "上海瑞琦实业有限公司合同.pdf",
            "size": 115845,
            "type": "application/pdf",
            "tmp_url": "https://open.feishu.cn/open-apis/drive/v1/medias/batch_get_tmp_download_url?file_tokens=...",
            "file_token": "D4t6bUMbdon9yCxvlnDcIkAJnkd"
          }
        ]
      }
    ],
    "page": 1,
    "page_size": 20,
    "total": 27,
    "pagination": { "page": 1, "page_size": 20, "total": 27, "has_next": false },
    "sort": { "sort_by": "_last_modified_time", "sort_order": "desc" }
  },
  "message": "ok"
}
```

**购销合同字段说明：**

| 字段名 | 说明 | 关键词搜索 | 备注 |
|-|-|-|-|
| `apply_no` | 申请编号 | ✅ | 已提取为纯文本，如 `"202604020046"` |
| `customer_name` | 客户名称 | ✅ | 映射自「交货地点*_客户名称」 |
| `supplier` | 供方 | ✅ | |
| `product_name` | 产品名称 | ✅ | |
| `contract_no` | 合同编号 | ✅ | |
| `spec` | 规格 | — | JSON 数组，如 `["HD810"]` |
| `quantity_kg` | 数量（kg） | — | JSON 数组（可能多批次） |
| `unit_price` | 单价（元/kg） | — | 数值 |
| `tax_total_amount` | 价税合计 | — | 数值 |
| `order_status` | 申请状态 | — | 审批中 / 已通过 等（不含已撤回/已拒绝） |
| `approval_node` | 审批节点 | — | |
| `approval_flow` | 审批流程 | — | |
| `ship_status` | 发货状态 | — | JSON 数组 |
| `stock_status` | 是否出库 | — | |
| `invoice_status` | 是否开票 | — | |
| `sales_user` | 销售员 | — | **已提取为纯名字**，如 `"张有连"` |
| `remark` | 备注 | — | |
| `source_id` | SourceID | — | |
| `ai_contract_upload` | AI合同上传附件 | — | 附件对象数组（含下载 url） |

> ⚠️ **按销售员查询**用字段过滤 `sales_user=张有连`（不在 `q` 关键词范围）。`q` 关键词只匹配：申请编号、客户名称、产品名称、合同编号、供方。

### 6.2 查询单个合同

```
GET /api/db/purchase-contracts/{record_id}
```

**示例：** `GET /api/db/purchase-contracts/recvfCE6ZVDLjK`

### 6.3 新增与写保护（rhcrm-server feat_v0.1 + D-007）

```
POST   /api/db/purchase-contracts          → 201；必填 apply_no；未来按 apply_no upsert（U2）
PATCH  /api/db/purchase-contracts/{id}     → 403（受保护）
DELETE /api/db/purchase-contracts/{id}     → 403（受保护）
```

---

## 7. 调用示例

### 7.1 curl 示例

```bash
# 健康检查（免鉴权）
curl "https://api.renheng.com/pgapi/health"

# 查询产品列表
curl "https://api.renheng.com/pgapi/api/db/products/search?page=1&page_size=10" \
  -H "X-API-Key: pgapi_1da296aaed0e574ae0f9b2a1d805a3b4ab542a89"

# 产品关键词搜索
curl "https://api.renheng.com/pgapi/api/db/products/search?q=BT&page=1&page_size=10" \
  -H "X-API-Key: pgapi_1da296aaed0e574ae0f9b2a1d805a3b4ab542a89"

# 查询客户列表（中文关键词）
curl -G "https://api.renheng.com/pgapi/api/db/customers/search" \
  -H "X-API-Key: pgapi_1da296aaed0e574ae0f9b2a1d805a3b4ab542a89" \
  --data-urlencode "q=科技" \
  --data-urlencode "page=1" \
  --data-urlencode "page_size=10"

# 客户按字段过滤
curl "https://api.renheng.com/pgapi/api/db/customers/search?customer_name=科技&page_size=10" \
  -H "X-API-Key: pgapi_1da296aaed0e574ae0f9b2a1d805a3b4ab542a89"

# 查询客户地址
curl "https://api.renheng.com/pgapi/api/db/customers/rec_xxx/addresses" \
  -H "X-API-Key: pgapi_1da296aaed0e574ae0f9b2a1d805a3b4ab542a89"

# 新增产品
curl -X POST "https://api.renheng.com/pgapi/api/db/products" \
  -H "X-API-Key: pgapi_1da296aaed0e574ae0f9b2a1d805a3b4ab542a89" \
  -H "Content-Type: application/json" \
  -d '{"product_name":"BT-38","spec":"25kg","category_major":"造纸化学品","is_mortar":false}'

# 新增客户
curl -X POST "https://api.renheng.com/pgapi/api/db/customers" \
  -H "X-API-Key: pgapi_1da296aaed0e574ae0f9b2a1d805a3b4ab542a89" \
  -H "Content-Type: application/json" \
  -d '{"customer_name":"某某科技有限公司","customer_level":"A","customer_type":"企业客户","supplier":"浙江长安仁恒科技股份有限公司","phone":"13800000000"}'

# 修改产品
curl -X PATCH "https://api.renheng.com/pgapi/api/db/products/rec_xxx" \
  -H "X-API-Key: pgapi_1da296aaed0e574ae0f9b2a1d805a3b4ab542a89" \
  -H "Content-Type: application/json" \
  -d '{"spec":"20kg"}'

# 修改客户
curl -X PATCH "https://api.renheng.com/pgapi/api/db/customers/rec_xxx" \
  -H "X-API-Key: pgapi_1da296aaed0e574ae0f9b2a1d805a3b4ab542a89" \
  -H "Content-Type: application/json" \
  -d '{"phone":"13900000000"}'
```

### 7.2 Node.js 调用示例（企微应用）

```js
const PGAPI_BASE = "https://api.renheng.com/pgapi";
const PGAPI_KEY = "pgapi_1da296aaed0e574ae0f9b2a1d805a3b4ab542a89";

async function pgapiGet(path) {
  const res = await fetch(`${PGAPI_BASE}${path}`, {
    headers: { "X-API-Key": PGAPI_KEY },
  });
  if (!res.ok) throw new Error(`pgAPI ${res.status}: ${await res.text()}`);
  return res.json();
}

async function pgapiPost(path, body) {
  const res = await fetch(`${PGAPI_BASE}${path}`, {
    method: "POST",
    headers: {
      "X-API-Key": PGAPI_KEY,
      "Content-Type": "application/json",
    },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new Error(`pgAPI ${res.status}: ${await res.text()}`);
  return res.json();
}

// 查询产品
const products = await pgapiGet("/api/db/products/search?q=BT&page_size=10");
console.log(products.data.items);

// 查询客户
const customers = await pgapiGet("/api/db/customers/search?customer_name=科技&page_size=10");
console.log(customers.data.items);

// 按销售员查询合同订单（分页）
const contracts = await pgapiGet("/api/db/purchase-contracts/search?sales_user=张有连&page=1&page_size=20");
console.log(contracts.data.items);
console.log("总合同数:", contracts.data.total);

// 新增客户
const created = await pgapiPost("/api/db/customers", {
  customer_name: "某某科技有限公司",
  customer_level: "A",
  customer_type: "企业客户",
  supplier: "浙江长安仁恒科技股份有限公司",
});
```

---

## 8. 注意事项

1. **鉴权**：业务接口必须带 `X-API-Key` 请求头，否则返回 401。
2. **写接口风险**：POST / PATCH 直接修改 PostgreSQL 生产库，接入前务必在测试环境验证。
3. **新增必填字段**：
   - 产品池：`product_name`、`spec`
   - 客户池：`customer_name`、`customer_level`、`customer_type`、`supplier`
4. **删除限制**：客户、产品当前均不支持删除（返回 400）；购销合同为只读资源（写操作返回 403）。
5. **关键词搜索范围**：
   - 产品池：`product_name`、`spec`、`tech_spec`、`category_major`、`category_minor`
   - 客户池：`customer_no`、`customer_name`、`tax_no`、`invoice_tax_no`、`phone`
   - 购销合同：`apply_no`、`customer_name`、`product_name`、`contract_no`、`supplier`
   - 供方（`supplier`）、销售员（`sales_user`）不在关键词范围，需用字段过滤
6. **购销合同默认过滤**：查询自动排除「已撤回」「已拒绝」状态记录。
7. **中文参数**：URL 编码（`q=科技` → `%E7%A7%91%E6%8A%80`）。
8. **数据口径**：当前数据以飞书多维表格同步为准（最近一次全量同步 2026-08-17），查询默认不含软删除记录。
9. **API Key 安全**：请存入服务端配置/环境变量，勿硬编码在前端或提交到代码仓库。
