// Package repo —— PostgreSQL 访问层（pgx），只服务流程引擎数据
// （orders/order_items/order_events/stock_*/shipment_*/invoice_*/approval_*/
// pre_order_*/evidence/outbox 等；docs/02-architecture.html §2.1/§3-5）。
// 订单数据实体（products/customers/purchase-contracts）经 internal/orderdata → pgAPI 访问。
// 防线落地：CHECK 约束（DB）、行锁 SELECT FOR UPDATE、UNIQUE 幂等、单事务守恒、事件链 append-only。
package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound 资源不存在。
type ErrNotFound struct{ What string }

func (e *ErrNotFound) Error() string { return "not found: " + e.What }

// IsNotFound 判断错误是否为资源不存在。
func IsNotFound(err error) bool {
	var e *ErrNotFound
	return errors.As(err, &e)
}

// ErrIdempotencyConflict 同幂等键异 payload（409）。
type ErrIdempotencyConflict struct {
	Key string
}

func (e *ErrIdempotencyConflict) Error() string {
	return fmt.Sprintf("idempotency conflict: key %q with different payload", e.Key)
}

// IsIdempotencyConflict 判断错误是否为幂等冲突。
func IsIdempotencyConflict(err error) bool {
	var e *ErrIdempotencyConflict
	return errors.As(err, &e)
}

// ErrAlreadyExists 资源已存在（唯一键冲突，非同键异 payload 场景）。
type ErrAlreadyExists struct{ Msg string }

func (e *ErrAlreadyExists) Error() string { return "already exists: " + e.Msg }

// Repo 持久层入口。
type Repo struct {
	pool *pgxpool.Pool
}

// New 建立连接池。
func New(ctx context.Context, dsn string) (*Repo, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.New: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgx ping: %w", err)
	}
	return &Repo{pool: pool}, nil
}

// Close 关闭连接池。
func (r *Repo) Close() { r.pool.Close() }

// Pool 暴露底层连接池（脚本直连场景）。
func (r *Repo) Pool() *pgxpool.Pool { return r.pool }

// Tx 事务包装。
type Tx struct {
	tx pgx.Tx
}

// Begin 开启事务。
func (r *Repo) Begin(ctx context.Context) (*Tx, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	return &Tx{tx: tx}, nil
}

// Commit 提交事务。
func (t *Tx) Commit(ctx context.Context) error { return t.tx.Commit(ctx) }

// Rollback 回滚事务。
func (t *Tx) Rollback(ctx context.Context) error { return t.tx.Rollback(ctx) }

// now 统一时间源。
func now() time.Time { return time.Now().UTC() }
