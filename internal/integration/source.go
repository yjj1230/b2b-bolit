package integration

// ── 动态数据源（Dynamic Data Source）契约 ────────────────────────────────
//
// 承接外部系统的实时/近实时数据接入。当前已落地的动态数据源：
//
//   - 企业微信（企微会话存档拉取 / 出站卡片 / 回调验签）：
//     现有实现为 internal/wecom.Gateway（docs/07-wecom-integration.html §2/§4），
//     已按 provider 隔离（mock | official | thirdparty），是接入层第一个动态源。
//
// 预留的动态数据源（docs/02-architecture.html §8 数据迁移布局揭示）：
//
//   - 飞书多维表格（bitable_flow_records / customer_bitable_sync）——
//     迁移源为 JSONL 追加式存储 + 多维表格表头（spec/biz/），后续若作为
//     动态业务数据源或配置源，实现本包接口注册即可，业务代码零改动。
//
// 迁移源（legacy_* 只读归档）不归入本层：用户决策归独立 internal/migration/。

// DynamicSource 动态数据源标识接口：所有外部数据源实现此接口并注册。
type DynamicSource interface {
	// SourceName 数据源名称（如 "wecom" / "feishu_bitable"）。
	SourceName() string
}

// Registry 动态数据源注册表：按名称取用，业务代码经此访问外部数据。
// 基线从配置注入（cmd/* main），不引入全局可变状态。
type Registry struct {
	sources map[string]DynamicSource
}

// NewRegistry 构造空注册表。
func NewRegistry() *Registry {
	return &Registry{sources: make(map[string]DynamicSource)}
}

// Register 注册数据源（同名覆盖）。
func (r *Registry) Register(s DynamicSource) {
	r.sources[s.SourceName()] = s
}

// Get 按名称取数据源；未注册返回 nil。
func (r *Registry) Get(name string) DynamicSource {
	return r.sources[name]
}
