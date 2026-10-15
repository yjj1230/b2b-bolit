// Package sync —— 飞书 → 本系统单向只读同步（docs/v0.1/04-architecture §6 / 11-migration）
//
// 0.1 上线方式：原审批流不停，飞书侧数据（数据库/多维表/三方审批流）单向只读同步进本系统，
// 商务在本系统流里可见可操作；0.1 不回写飞书。
// 调研清单 T1-T3 完成前，同步 Job 按「结构未知以配置驱动」原则开发：表名/字段/游标均可配置，
// 调研结果回填配置即可，不阻塞上线窗口。
package sync

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── 配置（结构未知以配置驱动；11-migration §3）──

// SourceConfig 单个同步源配置。
type SourceConfig struct {
	TenantID string `json:"tenant_id,omitempty"` // 覆盖全局租户（多租户扩展）
	Name     string `json:"name"`                // 源名（如 feishu_orders）
	Kind     string `json:"kind"`                // file | http
	Path     string `json:"path"`                // file: JSONL 文件路径；http: URL
	APIKey   string `json:"api_key,omitempty"`   // http: X-API-Key（pgAPI 源）
	PageSize int    `json:"page_size,omitempty"` // http: 分页大小（默认 200，最大 200）
	// 增量游标字段（源行内的时间/序号列；空 = 全量）
	CursorField string `json:"cursor_field"`
	// 字段映射：源列 → 目标订单字段（order_id/customer_id/state/payment_term/
	// item_product_id/item_quantity/item_unit_price_cents/legacy_flow_id/legacy_stage_id/
	// legacy_instance_id/legacy_task_id/operator_id/occurred_at/reason）
	ColumnMap map[string]string `json:"column_map"`
	// JoinSource 关联快照源名（如 feishu_orders 的 orders.jsonl）；JoinKeyField 本行
	// 用于关联快照的键（如 instance_id）；JoinFields 源列 → 快照列（flow_id/stage_code/order_id）。
	JoinSource   string            `json:"join_source,omitempty"`
	JoinKeyField string            `json:"join_key_field,omitempty"`
	JoinFields   map[string]string `json:"join_fields,omitempty"`
}

// Config 同步配置。
type Config struct {
	TenantID string         `json:"tenant_id"`
	Sources  []SourceConfig `json:"sources"`
}

// LoadConfig 读取 JSON 配置。
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if c.TenantID == "" {
		return nil, fmt.Errorf("tenant_id required")
	}
	return &c, nil
}

// ── 拉取（增量游标）──

// Row 源行（列名 → 字符串值）。
type Row map[string]string

// Batch 一批源行。
type Batch struct {
	Rows   []Row
	Cursor string // 拉取后的游标（源侧 updated_at/seq 最大值）
}

// Fetcher 源拉取契约。
type Fetcher interface {
	Fetch(ctx context.Context, since string) (*Batch, error)
}

// FileFetcher JSONL 文件源（调研完成前的配置驱动基线；每行一个 JSON 对象）。
type FileFetcher struct {
	Path        string
	CursorField string
}

// Fetch 读 JSONL：since 之后的行（游标字段字典序比较）；文件不存在 → 空批次。
func (f *FileFetcher) Fetch(ctx context.Context, since string) (*Batch, error) {
	file, err := os.Open(f.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Batch{}, nil
		}
		return nil, err
	}
	defer file.Close()
	out := &Batch{}
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, fmt.Errorf("parse line: %w", err)
		}
		// 标量转字符串；嵌套对象/数组紧凑序列化后保留在 payload 里，
		// 使 FileFetcher 能读取旧系统真实 JSONL（event.detail 为对象）。
		row := Row{}
		for k, v := range raw {
			row[k] = stringifyValue(v)
		}
		cur := row[f.CursorField]
		if since != "" && cur <= since {
			continue
		}
		out.Rows = append(out.Rows, row)
		if cur > out.Cursor {
			out.Cursor = cur
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// HTTPFetcher 远程 pgAPI 列表源（docs/发布计划v2 第 1 次发布 G4）：
// GET {URL}?page=&page_size=&sort_by=<CursorField>&sort_order=desc 分页拉取，
// 按 CursorField（默认 last_modified_time）做 since 截断。
type HTTPFetcher struct {
	URL         string
	APIKey      string
	CursorField string
	PageSize    int
}

type httpListEnvelope struct {
	Success bool `json:"success"`
	Data    struct {
		Items      []map[string]any `json:"items"`
		Pagination struct {
			HasNext bool `json:"has_next"`
		} `json:"pagination"`
	} `json:"data"`
	Message   string `json:"message"`
	ErrorCode string `json:"error_code"`
}

// Fetch 拉取 pgAPI 列表（desc 分页，遇到 since 截断停止）。
func (f *HTTPFetcher) Fetch(ctx context.Context, since string) (*Batch, error) {
	if f.URL == "" {
		return nil, fmt.Errorf("http source: URL required")
	}
	cursor := f.CursorField
	if cursor == "" {
		cursor = "last_modified_time"
	}
	size := f.PageSize
	if size <= 0 {
		size = 200
	}
	if size > 200 {
		size = 200
	}
	out := &Batch{}
	for page := 1; ; page++ {
		u, err := url.Parse(f.URL)
		if err != nil {
			return nil, fmt.Errorf("http source parse url: %w", err)
		}
		q := u.Query()
		q.Set("page", strconv.Itoa(page))
		q.Set("page_size", strconv.Itoa(size))
		q.Set("sort_by", cursor)
		q.Set("sort_order", "desc")
		u.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("http source request: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		if f.APIKey != "" {
			req.Header.Set("X-API-Key", f.APIKey)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("http source %s: %w", f.URL, err)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("http source read: %w", err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("http source %s status %d: %s", f.URL, resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		var env httpListEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, fmt.Errorf("http source decode: %w", err)
		}
		if !env.Success {
			return nil, fmt.Errorf("http source business error: code=%s message=%s", env.ErrorCode, env.Message)
		}
		stopped := false
		for _, item := range env.Data.Items {
			row := Row{}
			for k, v := range item {
				row[k] = stringifyValue(v)
			}
			cur := row[cursor]
			if since != "" && cur != "" && cur <= since {
				stopped = true
				continue
			}
			out.Rows = append(out.Rows, row)
			if cur > out.Cursor {
				out.Cursor = cur
			}
		}
		if stopped || !env.Data.Pagination.HasNext {
			return out, nil
		}
	}
}

func stringifyValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// NewFetcher 按配置构造拉取器。
func NewFetcher(sc SourceConfig) (Fetcher, error) {
	switch sc.Kind {
	case "file":
		return &FileFetcher{Path: sc.Path, CursorField: sc.CursorField}, nil
	case "http":
		return &HTTPFetcher{URL: sc.Path, APIKey: sc.APIKey, CursorField: sc.CursorField, PageSize: sc.PageSize}, nil
	default:
		return nil, fmt.Errorf("unknown source kind %q", sc.Kind)
	}
}

// ── 敏感字段边界（11-migration §6：业务库零留存红线）──

// sensitivePatterns 敏感字段正则（V4 验收：PG dump 静态扫描零命中）。
var sensitivePatterns = []*regexp.Regexp{
	regexp.MustCompile("(?i)(open_?id|access_?token|view_?token|private_?key|secret|encoding_?aes_?key)"),
}

// SensitiveKey 判断列名是否为敏感字段（不入库）。
func SensitiveKey(col string) bool {
	for _, p := range sensitivePatterns {
		if p.MatchString(col) {
			return true
		}
	}
	return false
}

// SanitizeRow 剥离敏感列（V4：open_id/token/secret 等不进业务表）。
// SYNC-03 收敛：值级防线——即使源列名不敏感，值中命中敏感模式也整体脱敏，
// 防止嵌套 JSON/自由文本把 token 带进 payload。
func SanitizeRow(row Row) Row {
	out := Row{}
	for k, v := range row {
		if SensitiveKey(k) {
			continue
		}
		if v != "" && SensitiveKey(v) {
			out[k] = "[REDACTED]"
			continue
		}
		out[k] = v
	}
	return out
}

// ── 映射（11-migration §5：flow_id → 订单主脊）──

// MappedOrder 映射后的订单记录（快照按业务主键取最后一条有效记录）。
type MappedOrder struct {
	TenantID        string
	OrderID         string
	CustomerID      string
	State           string
	PaymentTerm     string
	SupplierChannel string
	IdempotencyKey  string
	PayloadHash     string
	Payload         []byte
	LegacyFlowID    string
	// 事件链搬运（不得伪造审计链：event_id/occurred_at/action/task_id 来自源）
	EventAction   string
	EventReason   string
	EventOperator string
	EventTime     string
	// 迁移追溯（legacy_flow_mapping）
	LegacyStageID    string
	LegacyInstanceID string
	LegacyTaskID     string
	StageCode        string
}

// MapRow 源行 → 订单映射（配置 ColumnMap 决定列名；11 §5 映射要点）。
func MapRow(cfg SourceConfig, row Row) (*MappedOrder, error) {
	col := func(target string) string { return row[cfg.ColumnMap[target]] }
	o := &MappedOrder{
		TenantID:         cfg.TenantID,
		OrderID:          col("order_id"),
		CustomerID:       col("customer_id"),
		State:            col("state"),
		PaymentTerm:      col("payment_term"),
		SupplierChannel:  col("supplier_channel"),
		IdempotencyKey:   col("idempotency_key"),
		LegacyFlowID:     col("legacy_flow_id"),
		EventAction:      col("event_action"),
		EventReason:      col("event_reason"),
		EventOperator:    col("operator_id"),
		EventTime:        col("occurred_at"),
		LegacyStageID:    col("legacy_stage_id"),
		LegacyInstanceID: col("legacy_instance_id"),
		LegacyTaskID:     col("legacy_task_id"),
		StageCode:        col("stage_code"),
	}
	if o.LegacyFlowID == "" {
		return nil, fmt.Errorf("row missing legacy_flow_id")
	}
	if o.OrderID == "" {
		o.OrderID = "ORD-" + ShortID("legacy:"+o.LegacyFlowID)
	}
	if o.IdempotencyKey == "" {
		// 原键直接搬入（补租户前缀）；无原键按 flow_id 派生（11 §5 幂等键构造）
		o.IdempotencyKey = "legacy:" + o.LegacyFlowID
	}
	if o.PaymentTerm == "" {
		o.PaymentTerm = "款到" // 默认；源提供时以源为准
	} else {
		// 归一化：飞书结算方式细粒度值（月结30天/货到付款/票到60天…）→ b2b 枚举
		// （orders.payment_term CHECK 仅允许 款到 / 非款到）
		if strings.Contains(o.PaymentTerm, "款到") {
			o.PaymentTerm = "款到"
		} else {
			o.PaymentTerm = "非款到"
		}
	}
	if o.StageCode == "" {
		o.StageCode = "ORD"
	}
	payload, err := json.Marshal(row)
	if err != nil {
		return nil, err
	}
	o.Payload = payload
	o.PayloadHash = ShortID(string(payload))
	return o, nil
}

// ShortID 确定性短 ID（sha256 前 12 位 hex）。
func ShortID(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// ── 幂等批次（11-migration §4：migration_batches checksum 锚点，可重跑不重复）──

// BatchChecksum 批次校验和：按行排序后的规范 JSON 摘要。
func BatchChecksum(rows []Row) string {
	keys := make([]string, 0, len(rows))
	enc := map[string]string{}
	for i, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			b = []byte("{}")
		}
		keys = append(keys, string(b)+fmt.Sprintf(":%d", i))
		enc[keys[i]] = string(b)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(enc[k])
		sb.WriteString("\n")
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}

// ── 时间工具 ──

// ParseEventTime 解析事件时间（RFC3339；空 → 零值，写入时用 now()）。
func ParseEventTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}
