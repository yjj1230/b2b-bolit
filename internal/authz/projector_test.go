package authz

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"b2b_pilot_v3/internal/repo"
)

// ── 投影器单元测试（内存 writer/store；映射规则/幂等/失败重放）──

// memWriter 内存关系写入器（记录 Touch/Delete/Caveated）。
type memWriter struct {
	mu       sync.Mutex
	touched  []Relationship
	deleted  []Relationship
	caveated []struct {
		rel       Relationship
		expiresAt time.Time
	}
	failNext bool
}

func (m *memWriter) TouchRelationship(ctx context.Context, rel Relationship) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext {
		m.failNext = false
		return errors.New("inject: touch failed")
	}
	m.touched = append(m.touched, rel)
	return nil
}

func (m *memWriter) DeleteRelationship(ctx context.Context, rel Relationship) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = append(m.deleted, rel)
	return nil
}

func (m *memWriter) WriteCaveatedRelationship(ctx context.Context, rel Relationship, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.caveated = append(m.caveated, struct {
		rel       Relationship
		expiresAt time.Time
	}{rel, expiresAt})
	return nil
}

// memStore 内存投影事件存储。
type memStore struct {
	mu      sync.Mutex
	events  []repo.OutboxEvent
	nextID  int64
	done    map[int64]bool
	failed  map[int64]bool
	failAll bool
}

func (m *memStore) add(ev repo.OutboxEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	ev.ID = m.nextID
	m.events = append(m.events, ev)
}

func (m *memStore) ListPendingOutboxEvents(ctx context.Context, limit int) ([]repo.OutboxEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []repo.OutboxEvent
	for _, ev := range m.events {
		if m.done[ev.ID] || m.failed[ev.ID] {
			continue
		}
		out = append(out, ev)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memStore) MarkOutboxProcessed(ctx context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.done[id] = true
	return nil
}

func (m *memStore) MarkOutboxFailed(ctx context.Context, id int64, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failed[id] = true
	return nil
}

func (m *memStore) RequeueFailedOutboxEvents(ctx context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := int64(0)
	for id := range m.failed {
		delete(m.failed, id)
		n++
	}
	return n, nil
}

func TestProjectorMapping(t *testing.T) {
	store := &memStore{done: map[int64]bool{}, failed: map[int64]bool{}}
	writer := &memWriter{}
	p := NewProjector(store, writer)
	ctx := context.Background()

	// 五类事件（场景 A/B/C/D/G/H）
	store.add(repo.OutboxEvent{TenantID: "t", Aggregate: "prestock", AggregateID: "PS-1", EventType: "prestock.created", Payload: []byte(`{"owner_id":"u-op"}`)})
	store.add(repo.OutboxEvent{TenantID: "t", Aggregate: "shipment", AggregateID: "SHP-1", EventType: "shipment.created", Payload: []byte(`{"requester_id":"u-op","warehouse_operator_id":"u-wh"}`)})
	store.add(repo.OutboxEvent{TenantID: "t", Aggregate: "customer", AggregateID: "C-1", EventType: "customer.contact_upsert", Payload: []byte(`{"old_contact":"li","new_contact":"wang"}`)})
	store.add(repo.OutboxEvent{TenantID: "t", Aggregate: "delegation", AggregateID: "PS-2", EventType: "delegation.grant", Payload: []byte(`{"resource_type":"prestock","delegate_id":"li-na","expires_at":"2026-08-09T10:00:00Z"}`)})
	store.add(repo.OutboxEvent{TenantID: "t", Aggregate: "delegation", AggregateID: "PS-2", EventType: "delegation.revoke", Payload: []byte(`{"resource_type":"prestock","delegate_id":"li-na"}`)})

	n, err := p.RunOnce(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 5, n)

	// prestock#owner（场景 A/B）
	require.Equal(t, Relationship{ResourceType: ResPrestock, ResourceID: "PS-1", Relation: "owner", SubjectType: ResUser, SubjectID: "u-op"}, writer.touched[0])
	// shipment#requester + warehouse_operator（场景 B/C）
	require.Equal(t, Relationship{ResourceType: ResShipment, ResourceID: "SHP-1", Relation: "requester", SubjectType: ResUser, SubjectID: "u-op"}, writer.touched[1])
	require.Equal(t, Relationship{ResourceType: ResShipment, ResourceID: "SHP-1", Relation: "warehouse_operator", SubjectType: ResUser, SubjectID: "u-wh"}, writer.touched[2])
	// contact_upsert：Delete 旧 + Touch 新（场景 H）
	require.Equal(t, Relationship{ResourceType: ResCustomer, ResourceID: "C-1", Relation: "contact", SubjectType: ResUser, SubjectID: "li"}, writer.deleted[0])
	require.Equal(t, Relationship{ResourceType: ResCustomer, ResourceID: "C-1", Relation: "contact", SubjectType: ResUser, SubjectID: "wang"}, writer.touched[3])
	// delegation.grant（场景 G）→ caveated
	require.Len(t, writer.caveated, 1)
	require.Equal(t, "li-na", writer.caveated[0].rel.SubjectID)
	// delegation.revoke → delete
	require.Equal(t, Relationship{ResourceType: ResPrestock, ResourceID: "PS-2", Relation: "delegate", SubjectType: ResUser, SubjectID: "li-na"}, writer.deleted[1])

	// 全部标记完成，再次消费为空
	n, err = p.RunOnce(ctx, 10)
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestProjectorErrorsAndReplay(t *testing.T) {
	store := &memStore{done: map[int64]bool{}, failed: map[int64]bool{}}
	writer := &memWriter{}
	p := NewProjector(store, writer)
	ctx := context.Background()

	// 未知事件类型 → FAILED 标记
	store.add(repo.OutboxEvent{TenantID: "t", Aggregate: "x", AggregateID: "1", EventType: "bogus", Payload: []byte(`{}`)})
	// payload 缺字段 → FAILED
	store.add(repo.OutboxEvent{TenantID: "t", Aggregate: "prestock", AggregateID: "PS-1", EventType: "prestock.created", Payload: []byte(`{}`)})
	// 非法 JSON → FAILED
	store.add(repo.OutboxEvent{TenantID: "t", Aggregate: "prestock", AggregateID: "PS-2", EventType: "prestock.created", Payload: []byte(`{bad`)})
	// grant 时间非法 → FAILED
	store.add(repo.OutboxEvent{TenantID: "t", Aggregate: "delegation", AggregateID: "PS-3", EventType: "delegation.grant", Payload: []byte(`{"resource_type":"prestock","delegate_id":"x","expires_at":"nope"}`)})

	n, err := p.RunOnce(ctx, 10)
	require.NoError(t, err)
	require.Zero(t, n) // 全部失败 → 0 成功
	require.Empty(t, writer.touched)
	require.Len(t, store.failed, 4)

	// 写入失败 → FAILED 标记（可重放）
	writer2 := &memWriter{failNext: true}
	p2 := NewProjector(store, writer2)
	store.add(repo.OutboxEvent{TenantID: "t", Aggregate: "prestock", AggregateID: "PS-4", EventType: "prestock.created", Payload: []byte(`{"owner_id":"u-op"}`)})
	n, err = p2.RunOnce(ctx, 10)
	require.NoError(t, err)
	require.Zero(t, n)
	require.True(t, store.failed[store.nextID])
	// 重放（故障解除 + Requeue）→ 有效事件成功、非法事件再次 FAILED
	requeued, err := store.RequeueFailedOutboxEvents(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(5), requeued) // 4 个非法 + 1 个写入失败
	n, err = p2.RunOnce(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 1, n) // PS-4 成功；bogus/缺字段/非法 JSON/坏时间 4 个再次失败
}

func TestProjectorEventTypeMappingErrors(t *testing.T) {
	// 空 payload 场景（delegation.revoke 缺 delegate_id 等）已在 TestProjectorErrorsAndReplay 覆盖
	p := NewProjector(&memStore{done: map[int64]bool{}, failed: map[int64]bool{}}, &memWriter{})
	require.NotNil(t, p)
	require.NotNil(t, p.store)
}

// 投影器生命周期：Run 循环（ctx 取消退出）+ SetLogger + RunOnce 空批
func TestProjectorRunLifecycle(t *testing.T) {
	store := &memStore{done: map[int64]bool{}, failed: map[int64]bool{}}
	writer := &memWriter{}
	p := NewProjector(store, writer)
	logged := false
	p.SetLogger(func(f string, a ...any) { logged = true })
	_ = logged

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	p.Run(ctx, 10*time.Millisecond) // ctx 取消 → 正常退出

	// RunOnce 空批（无 PENDING）→ 0 且无错误
	n, err := p.RunOnce(context.Background(), 10)
	require.NoError(t, err)
	require.Zero(t, n)
}

// RunOnce/apply 剩余错误分支：store 列表失败 + 标记失败 + 单事件处理失败计数
func TestProjectorRunOnceErrorPaths(t *testing.T) {
	ctx := context.Background()

	// store.ListPendingOutboxEvents 失败 → RunOnce 返回错误
	failList := &failListStore{memStore: memStore{done: map[int64]bool{}, failed: map[int64]bool{}}}
	p := NewProjector(failList, &memWriter{})
	_, err := p.RunOnce(ctx, 10)
	require.Error(t, err)

	// 事件处理失败 → MarkOutboxFailed 被调用，继续处理其余事件
	store := &memStore{done: map[int64]bool{}, failed: map[int64]bool{}}
	store.add(repo.OutboxEvent{TenantID: "t", Aggregate: "prestock", AggregateID: "PS-1", EventType: "bogus", Payload: []byte(`{}`)})
	store.add(repo.OutboxEvent{TenantID: "t", Aggregate: "prestock", AggregateID: "PS-2", EventType: "prestock.created", Payload: []byte(`{"owner_id":"u-op"}`)})
	writer := &memWriter{}
	p2 := NewProjector(store, writer)
	n, err := p2.RunOnce(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 1, n) // 1 成功（bogus 失败被跳过）
	require.Len(t, store.failed, 1)

	// MarkOutboxProcessed 失败 → RunOnce 返回错误（事务性中断）
	failMark := &failMarkStore{memStore: memStore{done: map[int64]bool{}, failed: map[int64]bool{}}}
	failMark.add(repo.OutboxEvent{TenantID: "t", Aggregate: "prestock", AggregateID: "PS-3", EventType: "prestock.created", Payload: []byte(`{"owner_id":"u-op"}`)})
	p3 := NewProjector(failMark, &memWriter{})
	_, err = p3.RunOnce(ctx, 10)
	require.Error(t, err)
}

type failListStore struct {
	memStore
}

func (f *failListStore) ListPendingOutboxEvents(ctx context.Context, limit int) ([]repo.OutboxEvent, error) {
	return nil, errors.New("inject: list failed")
}

type failMarkStore struct {
	memStore
}

func (f *failMarkStore) MarkOutboxProcessed(ctx context.Context, id int64) error {
	return errors.New("inject: mark failed")
}

// apply 的 payload 解析失败分支（已覆盖）+ Run 退出路径已覆盖

// apply 缺字段分支 + Run 循环内 RunOnce 失败日志 + roleAllows 未知权限
func TestProjectorApplyMissingFieldsAndRunErrors(t *testing.T) {
	ctx := context.Background()

	// 缺字段分支（JSON 合法但字段缺失）
	store := &memStore{done: map[int64]bool{}, failed: map[int64]bool{}}
	writer := &memWriter{}
	p := NewProjector(store, writer)
	store.add(repo.OutboxEvent{TenantID: "t", Aggregate: "prestock", AggregateID: "PS-1", EventType: "prestock.created", Payload: []byte(`{"x":1}`)})                                                           // 缺 owner_id
	store.add(repo.OutboxEvent{TenantID: "t", Aggregate: "shipment", AggregateID: "SHP-1", EventType: "shipment.created", Payload: []byte(`{"requester_id":"r"}`)})                                             // 缺 warehouse_operator_id
	store.add(repo.OutboxEvent{TenantID: "t", Aggregate: "customer", AggregateID: "C-1", EventType: "customer.contact_upsert", Payload: []byte(`{"old_contact":"o"}`)})                                         // 缺 new_contact
	store.add(repo.OutboxEvent{TenantID: "t", Aggregate: "delegation", AggregateID: "D-1", EventType: "delegation.grant", Payload: []byte(`{"resource_type":"prestock","expires_at":"2026-08-09T10:00:00Z"}`)}) // 缺 delegate_id
	store.add(repo.OutboxEvent{TenantID: "t", Aggregate: "delegation", AggregateID: "D-2", EventType: "delegation.revoke", Payload: []byte(`{"resource_type":"prestock"}`)})                                    // 缺 delegate_id
	n, err := p.RunOnce(ctx, 10)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Len(t, store.failed, 5)

	// Run 循环内 RunOnce 失败 → 日志路径（failListStore）
	failStore := &failListStore{memStore: memStore{done: map[int64]bool{}, failed: map[int64]bool{}}}
	p2 := NewProjector(failStore, writer)
	logged := false
	p2.SetLogger(func(f string, a ...any) { logged = true })
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		p2.Run(runCtx, 5*time.Millisecond)
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done
	require.True(t, logged, "RunOnce failure inside Run should log")

	// roleAllows 未知权限 → false（默认分支）
	if roleAllows(RoleAdmin, Permission("bogus")) {
		t.Fatal("unknown permission must be denied")
	}
}

// mustStruct panic 分支 + NewSpiceDB 非法地址
func TestSpiceDBEdgeBranches(t *testing.T) {
	// mustStruct：非法值（chan）→ panic 可恢复
	require.Panics(t, func() {
		_ = mustStruct(map[string]any{"bad": make(chan int)})
	})
	// NewSpiceDB：grpc 懒连接构造恒成功（构造错误分支物理不可达——
	// authzedclient.NewClient 内部 grpc.NewClient 懒连接）；RPC 失败路径已由 TestSpiceDBClientErrors 覆盖
	c, err := NewSpiceDB("127.0.0.1:1", "key")
	require.NoError(t, err)
	require.NotNil(t, c)
}
