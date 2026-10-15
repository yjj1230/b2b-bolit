// Package authz —— 授权层（docs/02-architecture.html §2.3；决策 D4 Outbox 投影器）
package authz

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"b2b_pilot_v3/internal/repo"
)

// ── Outbox 投影器（Transactional Outbox → SpiceDB 关系同步）──
//
// 业务事务内写 outbox_events → 投影器异步消费 → Touch/Delete SpiceDB 关系元组。
// 幂等：Touch 本身幂等；Delete 不存在的关系不报错（SpiceDB 语义）；事件按 id 标记 PROCESSED。
// 投影规则（docs/02-architecture.html §2.3）：
//   prestock.created         → Touch prestock#owner
//   shipment.created         → Touch shipment#requester + shipment#warehouse_operator
//   customer.contact_upsert  → Delete customer#contact@old + Touch customer#contact@new（场景 H）
//   delegation.grant         → WriteCaveatedRelationship(delegate, expires_at)（场景 G）
//   delegation.revoke        → Delete delegate（场景 G TTL 到期/提前收回）

// RelationWriter SpiceDB 关系写入契约（SpiceDBClient 实现；测试用内存实现）。
type RelationWriter interface {
	TouchRelationship(ctx context.Context, rel Relationship) error
	DeleteRelationship(ctx context.Context, rel Relationship) error
	WriteCaveatedRelationship(ctx context.Context, rel Relationship, expiresAt time.Time) error
}

// OutboxStore 投影事件存储契约（repo.Repo 实现）。
type OutboxStore interface {
	ListPendingOutboxEvents(ctx context.Context, limit int) ([]repo.OutboxEvent, error)
	MarkOutboxProcessed(ctx context.Context, id int64) error
	MarkOutboxFailed(ctx context.Context, id int64, errMsg string) error
	RequeueFailedOutboxEvents(ctx context.Context) (int64, error)
}

// Projector Outbox 投影器。
type Projector struct {
	store  OutboxStore
	writer RelationWriter
	logf   func(format string, args ...any)
}

// NewProjector 构造投影器。
func NewProjector(store OutboxStore, writer RelationWriter) *Projector {
	return &Projector{store: store, writer: writer, logf: log.Printf}
}

// SetLogger 注入日志。
func (p *Projector) SetLogger(f func(string, ...any)) { p.logf = f }

// RunOnce 消费一批待投影事件；返回处理数量（失败事件标记 FAILED 可重放）。
func (p *Projector) RunOnce(ctx context.Context, limit int) (int, error) {
	events, err := p.store.ListPendingOutboxEvents(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("list outbox: %w", err)
	}
	processed := 0
	for _, ev := range events {
		if err := p.apply(ctx, ev); err != nil {
			p.logf("outbox event %d (%s) failed: %v", ev.ID, ev.EventType, err)
			if ferr := p.store.MarkOutboxFailed(ctx, ev.ID, err.Error()); ferr != nil {
				return processed, ferr
			}
			continue
		}
		if err := p.store.MarkOutboxProcessed(ctx, ev.ID); err != nil {
			return processed, fmt.Errorf("mark outbox %d: %w", ev.ID, err)
		}
		processed++
	}
	return processed, nil
}

// Run 消费循环（worker 内运行）。
func (p *Projector) Run(ctx context.Context, interval time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			if _, err := p.RunOnce(ctx, 100); err != nil {
				p.logf("outbox run: %v", err)
			}
		}
	}
}

// apply 按事件类型映射到 SpiceDB 关系操作。
func (p *Projector) apply(ctx context.Context, ev repo.OutboxEvent) error {
	var payload map[string]any
	if len(ev.Payload) > 0 {
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			return fmt.Errorf("parse payload: %w", err)
		}
	}
	switch ev.EventType {
	case "prestock.created":
		ownerID, _ := payload["owner_id"].(string)
		if ownerID == "" {
			return fmt.Errorf("prestock.created payload 缺 owner_id")
		}
		return p.writer.TouchRelationship(ctx, Relationship{
			ResourceType: ResPrestock, ResourceID: ev.AggregateID,
			Relation: "owner", SubjectType: ResUser, SubjectID: ownerID,
		})
	case "shipment.created":
		requesterID, _ := payload["requester_id"].(string)
		operatorID, _ := payload["warehouse_operator_id"].(string)
		if requesterID == "" || operatorID == "" {
			return fmt.Errorf("shipment.created payload 缺 requester_id/warehouse_operator_id")
		}
		if err := p.writer.TouchRelationship(ctx, Relationship{
			ResourceType: ResShipment, ResourceID: ev.AggregateID,
			Relation: "requester", SubjectType: ResUser, SubjectID: requesterID,
		}); err != nil {
			return err
		}
		return p.writer.TouchRelationship(ctx, Relationship{
			ResourceType: ResShipment, ResourceID: ev.AggregateID,
			Relation: "warehouse_operator", SubjectType: ResUser, SubjectID: operatorID,
		})
	case "customer.contact_upsert":
		oldContact, _ := payload["old_contact"].(string)
		newContact, _ := payload["new_contact"].(string)
		if newContact == "" {
			return fmt.Errorf("contact_upsert payload 缺 new_contact")
		}
		if oldContact != "" && oldContact != newContact {
			if err := p.writer.DeleteRelationship(ctx, Relationship{
				ResourceType: ResCustomer, ResourceID: ev.AggregateID,
				Relation: "contact", SubjectType: ResUser, SubjectID: oldContact,
			}); err != nil {
				return err
			}
		}
		return p.writer.TouchRelationship(ctx, Relationship{
			ResourceType: ResCustomer, ResourceID: ev.AggregateID,
			Relation: "contact", SubjectType: ResUser, SubjectID: newContact,
		})
	case "delegation.grant":
		resourceType := ResourceType(fmt.Sprint(payload["resource_type"]))
		delegateID, _ := payload["delegate_id"].(string)
		expiresRaw, _ := payload["expires_at"].(string)
		if delegateID == "" || expiresRaw == "" {
			return fmt.Errorf("delegation.grant payload 缺 delegate_id/expires_at")
		}
		expiresAt, err := time.Parse(time.RFC3339, expiresRaw)
		if err != nil {
			return fmt.Errorf("parse expires_at: %w", err)
		}
		return p.writer.WriteCaveatedRelationship(ctx, Relationship{
			ResourceType: resourceType, ResourceID: ev.AggregateID,
			Relation: "delegate", SubjectType: ResUser, SubjectID: delegateID,
		}, expiresAt)
	case "delegation.revoke":
		resourceType := ResourceType(fmt.Sprint(payload["resource_type"]))
		delegateID, _ := payload["delegate_id"].(string)
		if delegateID == "" {
			return fmt.Errorf("delegation.revoke payload 缺 delegate_id")
		}
		return p.writer.DeleteRelationship(ctx, Relationship{
			ResourceType: resourceType, ResourceID: ev.AggregateID,
			Relation: "delegate", SubjectType: ResUser, SubjectID: delegateID,
		})
	default:
		return fmt.Errorf("unknown outbox event type %q", ev.EventType)
	}
}
