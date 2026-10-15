package repo

import (
	"context"
	"fmt"
	"time"

	"b2b_pilot_v3/internal/domain"
	"github.com/jackc/pgx/v5"
)

const (
	InboundReceived  = "RECEIVED"
	InboundParsed    = "PARSED"
	InboundFailed    = "FAILED"
	InboundIgnored   = "IGNORED"
	InboundSuspended = "SUSPENDED"

	ConversationOpen           = "OPEN"
	ConversationWaitingConfirm = "WAITING_CONFIRM"
	ConversationEscalated      = "ESCALATED"
	ConversationConfirmed      = "CONFIRMED"
	ConversationExpired        = "EXPIRED"
	ConversationManualRequired = "MANUAL_REQUIRED"
)

type ErrConversationAmbiguous struct {
	TenantID string
	GroupID  string
	Count    int
}

func (e *ErrConversationAmbiguous) Error() string {
	return fmt.Sprintf("ambiguous order conversation: tenant=%s group=%s candidates=%d", e.TenantID, e.GroupID, e.Count)
}

type NewInboundMessage struct {
	TenantID      string
	GroupID       string
	MsgID         string
	SenderID      string
	MessageSeq    *int64
	ReplyToMsgID  string
	RawText       string
	TextHash      string
	ReceivedAt    time.Time
	CorrelationID string
}

type NewOrderConversation struct {
	TenantID   string
	GroupID    string
	SenderID   string
	RootMsgID  string
	ExpiresAt  time.Time
	EscalateAt time.Time
}

type ConversationDraftInput struct {
	TenantID          string
	SessionID         string
	InboundMessageID  int64
	SourceMsgID       string
	SourceMsgHash     string
	Candidate         []byte
	SnapshotJSON      []byte
	PatchJSON         []byte
	Evidence          []byte
	Diff              []byte
	PolicyStatus      string
	ModelName         string
	ModelVersion      string
	PromptVersion     string
	PolicyVersion     string
	CreatedBy         string
	Status            string
	NextPreOrderState string
}

func (r *Repo) CreateInboundMessage(ctx context.Context, in NewInboundMessage) (*InboundMessage, bool, error) {
	if in.ReceivedAt.IsZero() {
		in.ReceivedAt = time.Now().UTC()
	}
	if in.TextHash == "" {
		in.TextHash = HashPayload([]byte(in.RawText))
	}
	if in.CorrelationID == "" {
		in.CorrelationID = "agent:" + in.TenantID + ":msg:" + in.MsgID
	}
	row := r.pool.QueryRow(ctx, `
		INSERT INTO inbound_messages
			(tenant_id, group_id, msg_id, sender_id, message_seq, reply_to_msg_id,
			 raw_text, text_hash, received_at, parse_status, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (tenant_id, group_id, msg_id) DO NOTHING
		RETURNING id, tenant_id, group_id, msg_id, sender_id, message_seq, reply_to_msg_id,
		          raw_text, text_hash, received_at, parse_status, retry_count, session_id,
		          draft_version, correlation_id, last_error, created_at, updated_at`,
		in.TenantID, in.GroupID, in.MsgID, in.SenderID, in.MessageSeq, in.ReplyToMsgID,
		in.RawText, in.TextHash, in.ReceivedAt, InboundReceived, in.CorrelationID)
	msg, err := scanInboundMessage(row)
	if err == nil {
		return msg, true, nil
	}
	if err != pgx.ErrNoRows {
		return nil, false, err
	}
	msg, err = r.GetInboundMessage(ctx, in.TenantID, in.GroupID, in.MsgID)
	return msg, false, err
}

func (r *Repo) GetInboundMessage(ctx context.Context, tenantID, groupID, msgID string) (*InboundMessage, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, tenant_id, group_id, msg_id, sender_id, message_seq, reply_to_msg_id,
		       raw_text, text_hash, received_at, parse_status, retry_count, session_id,
		       draft_version, correlation_id, last_error, created_at, updated_at
		FROM inbound_messages WHERE tenant_id=$1 AND group_id=$2 AND msg_id=$3`,
		tenantID, groupID, msgID)
	return scanInboundMessage(row)
}

func scanInboundMessage(row rowScanner) (*InboundMessage, error) {
	var out InboundMessage
	if err := row.Scan(&out.ID, &out.TenantID, &out.GroupID, &out.MsgID, &out.SenderID,
		&out.MessageSeq, &out.ReplyToMsgID, &out.RawText, &out.TextHash, &out.ReceivedAt,
		&out.ParseStatus, &out.RetryCount, &out.SessionID, &out.DraftVersion,
		&out.CorrelationID, &out.LastError, &out.CreatedAt, &out.UpdatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return nil, &ErrNotFound{What: "inbound_message"}
		}
		return nil, err
	}
	return &out, nil
}

func (r *Repo) FindOpenOrderConversation(ctx context.Context, tenantID, groupID, senderID string) (*OrderConversation, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT session_id, tenant_id, group_id, sender_id, customer_id, owner_sales_id,
		       pre_order_id, current_draft_version, last_draft_version, confirmed_draft_version,
		       status, last_message_at, escalate_at, expires_at, correlation_id, created_at, updated_at
		FROM order_conversations
		WHERE tenant_id=$1 AND group_id=$2 AND sender_id=$3
		  AND status IN ('OPEN','WAITING_CONFIRM','ESCALATED')
		  AND expires_at > now()
		ORDER BY updated_at DESC
		LIMIT 2`, tenantID, groupID, senderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var matches []OrderConversation
	for rows.Next() {
		var c OrderConversation
		if err := rows.Scan(&c.SessionID, &c.TenantID, &c.GroupID, &c.SenderID, &c.CustomerID,
			&c.OwnerSalesID, &c.PreOrderID, &c.CurrentDraftVersion, &c.LastDraftVersion,
			&c.ConfirmedDraftVersion, &c.Status, &c.LastMessageAt, &c.EscalateAt, &c.ExpiresAt,
			&c.CorrelationID, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		matches = append(matches, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch len(matches) {
	case 0:
		return nil, &ErrNotFound{What: "open order conversation"}
	case 1:
		return &matches[0], nil
	default:
		return nil, &ErrConversationAmbiguous{TenantID: tenantID, GroupID: groupID, Count: len(matches)}
	}
}

func (r *Repo) CreateOrderConversation(ctx context.Context, in NewOrderConversation) (*OrderConversation, *PreOrder, error) {
	if in.ExpiresAt.IsZero() {
		in.ExpiresAt = time.Now().UTC().Add(72 * time.Hour)
	}
	if in.EscalateAt.IsZero() {
		in.EscalateAt = time.Now().UTC().Add(24 * time.Hour)
	}
	sessionID := "SESSION-" + shortID(in.TenantID+":"+in.GroupID+":"+in.RootMsgID)
	correlationID := "agent:" + in.TenantID + ":" + sessionID
	preOrderID := "PRE-" + shortID(in.TenantID+":"+sessionID)
	tx, err := r.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	_, err = tx.tx.Exec(ctx, `
		INSERT INTO pre_orders
			(id, tenant_id, group_id, msg_id, state, candidates, idempotency_key, root_msg_id)
		VALUES ($1,$2,$3,$4,$5,'{}',$6,$4)
		ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`,
		preOrderID, in.TenantID, in.GroupID, in.RootMsgID, domain.StatePreCaptured,
		"conversation:"+in.TenantID+":"+sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("create conversation pre-order: %w", err)
	}
	_, err = tx.tx.Exec(ctx, `
		INSERT INTO order_conversations
			(session_id, tenant_id, group_id, sender_id, pre_order_id, status,
			 last_message_at, escalate_at, expires_at, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,now(),$7,$8,$9)
		ON CONFLICT (tenant_id, session_id) DO NOTHING`,
		sessionID, in.TenantID, in.GroupID, in.SenderID, preOrderID, ConversationOpen,
		in.EscalateAt, in.ExpiresAt, correlationID)
	if err != nil {
		return nil, nil, fmt.Errorf("create order conversation: %w", err)
	}
	if _, err := tx.tx.Exec(ctx, `
		UPDATE pre_orders SET conversation_id=$3, root_msg_id=$4, updated_at=now()
		WHERE tenant_id=$1 AND id=$2`, in.TenantID, preOrderID, sessionID, in.RootMsgID); err != nil {
		return nil, nil, fmt.Errorf("link conversation pre-order: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	c, err := r.GetOrderConversation(ctx, in.TenantID, sessionID)
	if err != nil {
		return nil, nil, err
	}
	p, err := r.GetPreOrder(ctx, in.TenantID, preOrderID)
	return c, p, err
}

func (r *Repo) GetOrderConversation(ctx context.Context, tenantID, sessionID string) (*OrderConversation, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT session_id, tenant_id, group_id, sender_id, customer_id, owner_sales_id,
		       pre_order_id, current_draft_version, last_draft_version, confirmed_draft_version,
		       status, last_message_at, escalate_at, expires_at, correlation_id, created_at, updated_at
		FROM order_conversations WHERE tenant_id=$1 AND session_id=$2`, tenantID, sessionID)
	var c OrderConversation
	if err := row.Scan(&c.SessionID, &c.TenantID, &c.GroupID, &c.SenderID, &c.CustomerID,
		&c.OwnerSalesID, &c.PreOrderID, &c.CurrentDraftVersion, &c.LastDraftVersion,
		&c.ConfirmedDraftVersion, &c.Status, &c.LastMessageAt, &c.EscalateAt, &c.ExpiresAt,
		&c.CorrelationID, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return nil, &ErrNotFound{What: "order_conversation"}
		}
		return nil, err
	}
	return &c, nil
}

// ApplyConversationDraft 在会话行锁下创建不可变版本，并把消息标记为已归并。
func (r *Repo) ApplyConversationDraft(ctx context.Context, in ConversationDraftInput) (*PreOrderDraft, *OrderConversation, error) {
	tx, err := r.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	var preOrderID, conversationStatus string
	var currentVersion, lastVersion int64
	var expiresAt time.Time
	if err := tx.tx.QueryRow(ctx, `
		SELECT pre_order_id, current_draft_version, last_draft_version, status, expires_at
		FROM order_conversations
		WHERE tenant_id=$1 AND session_id=$2 FOR UPDATE`, in.TenantID, in.SessionID).
		Scan(&preOrderID, &currentVersion, &lastVersion, &conversationStatus, &expiresAt); err != nil {
		return nil, nil, fmt.Errorf("lock order conversation: %w", err)
	}
	if conversationStatus == ConversationConfirmed || conversationStatus == ConversationExpired {
		return nil, nil, fmt.Errorf("order conversation %s is closed", in.SessionID)
	}
	var inboundSession *string
	if err := tx.tx.QueryRow(ctx, `
		SELECT session_id FROM inbound_messages WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
		in.TenantID, in.InboundMessageID).Scan(&inboundSession); err != nil {
		return nil, nil, fmt.Errorf("lock inbound message: %w", err)
	}
	if inboundSession != nil && *inboundSession != "" {
		draft, err := r.GetCurrentPreOrderDraft(ctx, in.TenantID, preOrderID)
		return draft, nil, err
	}
	version := lastVersion + 1
	draftStatus := in.Status
	if draftStatus == "" {
		draftStatus = "CURRENT"
	}
	if draftStatus == "CURRENT" {
		if _, err := tx.tx.Exec(ctx, `
			UPDATE pre_order_drafts SET status='SUPERSEDED'
			WHERE tenant_id=$1 AND pre_order_id=$2 AND status='CURRENT'`, in.TenantID, preOrderID); err != nil {
			return nil, nil, fmt.Errorf("supersede conversation draft: %w", err)
		}
		currentVersion = version
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO pre_order_drafts (
			tenant_id, pre_order_id, draft_version, source_msg_id, source_msg_hash,
			quote, evidence, candidate, policy_status, policy_result,
			customer_basis, product_basis, contract_basis, price_basis,
			model_name, model_version, prompt_version, policy_version, status,
			expires_at, diff, snapshot_json, patch_json, created_by)
		VALUES ($1,$2,$3,$4,$5,'{}',$6,$7,$8,'{}','{}','{}','{}','{}',
			$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		in.TenantID, preOrderID, version, in.SourceMsgID, in.SourceMsgHash,
		jsonOrEmpty(in.Evidence, "[]"), jsonOrEmpty(in.Candidate, "{}"), in.PolicyStatus,
		in.ModelName, in.ModelVersion, in.PromptVersion, in.PolicyVersion, draftStatus,
		expiresAt, jsonOrEmpty(in.Diff, "{}"), jsonOrEmpty(in.SnapshotJSON, "{}"),
		jsonOrEmpty(in.PatchJSON, "{}"), in.CreatedBy); err != nil {
		return nil, nil, fmt.Errorf("insert conversation draft: %w", err)
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO pre_order_draft_sources
			(tenant_id, pre_order_id, draft_version, message_id, source_role, quote)
		VALUES ($1,$2,$3,$4,'INPUT',$5)
		ON CONFLICT DO NOTHING`,
		in.TenantID, preOrderID, version, in.InboundMessageID, ""); err != nil {
		return nil, nil, fmt.Errorf("insert draft source: %w", err)
	}
	nextState := in.NextPreOrderState
	if nextState == "" {
		nextState = domain.StatePreCollecting
	}
	var oldState string
	if err := tx.tx.QueryRow(ctx, `
		SELECT state FROM pre_orders WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
		in.TenantID, preOrderID).Scan(&oldState); err != nil {
		return nil, nil, fmt.Errorf("lock conversation pre-order: %w", err)
	}
	if _, err := tx.tx.Exec(ctx, `
		UPDATE pre_orders
		SET candidates=CASE WHEN jsonb_typeof($3::jsonb)='array' AND jsonb_array_length($3::jsonb)>0
			THEN $3::jsonb ELSE candidates END,
		    state=$4, updated_at=now()
		WHERE tenant_id=$1 AND id=$2`, in.TenantID, preOrderID,
		jsonOrEmpty(in.Candidate, "[]"), nextState); err != nil {
		return nil, nil, fmt.Errorf("update conversation pre-order: %w", err)
	}
	if oldState != nextState {
		if err := r.appendPreOrderEvent(ctx, tx, in.TenantID, preOrderID, oldState, nextState,
			"ConversationDraftMerged", "多轮消息归并", in.Diff, nil, in.CreatedBy); err != nil {
			return nil, nil, err
		}
	}
	parseStatus := InboundParsed
	if draftStatus == "CONFLICT" {
		parseStatus = InboundSuspended
	}
	if _, err := tx.tx.Exec(ctx, `
		UPDATE inbound_messages
		SET session_id=$3, draft_version=$4, parse_status=$5, updated_at=now()
		WHERE tenant_id=$1 AND id=$2`, in.TenantID, in.InboundMessageID,
		in.SessionID, version, parseStatus); err != nil {
		return nil, nil, fmt.Errorf("mark inbound message: %w", err)
	}
	nextConversationStatus := ConversationOpen
	if draftStatus == "CONFLICT" {
		nextConversationStatus = ConversationManualRequired
	}
	if _, err := tx.tx.Exec(ctx, `
		UPDATE order_conversations
		SET current_draft_version=$3, last_draft_version=$4, last_message_at=now(),
		    escalate_at=now()+interval '24 hours', updated_at=now(), status=$5
		WHERE tenant_id=$1 AND session_id=$2`, in.TenantID, in.SessionID,
		currentVersion, version, nextConversationStatus); err != nil {
		return nil, nil, fmt.Errorf("update order conversation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	draft, err := r.GetPreOrderDraft(ctx, in.TenantID, preOrderID, version)
	if err != nil {
		return nil, nil, err
	}
	c, err := r.GetOrderConversation(ctx, in.TenantID, in.SessionID)
	return draft, c, err
}
