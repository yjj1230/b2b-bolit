package authz

import (
	"context"
	"fmt"
	"os"
	"time"

	authzed "github.com/authzed/authzed-go/proto/authzed/api/v1"
	authzedclient "github.com/authzed/authzed-go/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

// SpiceDBClient SpiceDB 客户端（FullyConsistent 读：撤销即删边、即时生效，CQ-13）。
type SpiceDBClient struct {
	client *authzedclient.Client
}

// NewSpiceDB 连接 SpiceDB（addr 如 127.0.0.1:50051，token 为 preshared key）。
func NewSpiceDB(addr, token string) (*SpiceDBClient, error) {
	client, err := authzedclient.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(&tokenCred{token: token}),
	)
	if err != nil {
		return nil, fmt.Errorf("connect spicedb %s: %w", addr, err)
	}
	return &SpiceDBClient{client: client}, nil
}

type tokenCred struct{ token string }

func (t *tokenCred) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + t.token}, nil
}
func (t *tokenCred) RequireTransportSecurity() bool { return false }

// ApplySchema 应用 schema 文本（编译门禁：语法错误在此暴露）。
func (c *SpiceDBClient) ApplySchema(ctx context.Context, schema string) error {
	_, err := c.client.WriteSchema(ctx, &authzed.WriteSchemaRequest{Schema: schema})
	return err
}

// ApplySchemaFromFile 从文件应用 schema。
func (c *SpiceDBClient) ApplySchemaFromFile(ctx context.Context, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return c.ApplySchema(ctx, string(b))
}

// Check 授权判定（FullyConsistent，无缓存）。
func (c *SpiceDBClient) Check(ctx context.Context, req CheckRequest) (bool, error) {
	checkReq := &authzed.CheckPermissionRequest{
		Consistency: &authzed.Consistency{
			Requirement: &authzed.Consistency_FullyConsistent{FullyConsistent: true},
		},
		Resource:   &authzed.ObjectReference{ObjectType: string(req.ResourceType), ObjectId: req.ResourceID},
		Permission: string(req.Permission),
		Subject:    &authzed.SubjectReference{Object: &authzed.ObjectReference{ObjectType: "user", ObjectId: req.SubjectID}},
	}
	// caveat expiry_ok 需要 `now`（v1.39 CEL 环境无内置时钟）
	if req.Now != nil {
		checkReq.Context = mustStruct(map[string]any{"now": req.Now.Format(time.RFC3339)})
	}
	resp, err := c.client.CheckPermission(ctx, checkReq)
	if err != nil {
		return false, fmt.Errorf("check %s#%s@%s: %w", req.ResourceType, req.Permission, req.SubjectID, err)
	}
	return resp.Permissionship == authzed.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION, nil
}

// TouchRelationship 写入关系（幂等 upsert 语义）。
func (c *SpiceDBClient) TouchRelationship(ctx context.Context, rel Relationship) error {
	update := c.relToUpdate(rel, authzed.RelationshipUpdate_OPERATION_TOUCH)
	_, err := c.client.WriteRelationships(ctx, &authzed.WriteRelationshipsRequest{Updates: []*authzed.RelationshipUpdate{update}})
	if err != nil {
		return fmt.Errorf("touch %s#%s@%s: %w", rel.ResourceType, rel.Relation, rel.SubjectID, err)
	}
	return nil
}

// DeleteRelationship 删除关系（撤销即失效）。
func (c *SpiceDBClient) DeleteRelationship(ctx context.Context, rel Relationship) error {
	update := c.relToUpdate(rel, authzed.RelationshipUpdate_OPERATION_DELETE)
	_, err := c.client.WriteRelationships(ctx, &authzed.WriteRelationshipsRequest{Updates: []*authzed.RelationshipUpdate{update}})
	if err != nil {
		return fmt.Errorf("delete %s#%s@%s: %w", rel.ResourceType, rel.Relation, rel.SubjectID, err)
	}
	return nil
}

// WriteCaveatedRelationship 写入带 caveat 的关系（代办授权，CQ-13）。
func (c *SpiceDBClient) WriteCaveatedRelationship(ctx context.Context, rel Relationship, expiresAt time.Time) error {
	update := &authzed.RelationshipUpdate{
		Operation: authzed.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &authzed.Relationship{
			Resource: &authzed.ObjectReference{ObjectType: string(rel.ResourceType), ObjectId: rel.ResourceID},
			Relation: rel.Relation,
			Subject:  &authzed.SubjectReference{Object: &authzed.ObjectReference{ObjectType: string(rel.SubjectType), ObjectId: rel.SubjectID}},
			OptionalCaveat: &authzed.ContextualizedCaveat{
				CaveatName: "expiry_ok",
				Context:    mustStruct(map[string]any{"expires_at": expiresAt.Format(time.RFC3339)}),
			},
		},
	}
	_, err := c.client.WriteRelationships(ctx, &authzed.WriteRelationshipsRequest{Updates: []*authzed.RelationshipUpdate{update}})
	if err != nil {
		return fmt.Errorf("write caveated %s#%s@%s: %w", rel.ResourceType, rel.Relation, rel.SubjectID, err)
	}
	return nil
}

func (c *SpiceDBClient) relToUpdate(rel Relationship, op authzed.RelationshipUpdate_Operation) *authzed.RelationshipUpdate {
	// SubjectType 默认 user：本项目 schema 的全部 subject 均为 user 类型
	// （schema.zed）；显式传其他类型（如群组）由调用方负责。
	subjectType := rel.SubjectType
	if subjectType == "" {
		subjectType = ResUser
	}
	return &authzed.RelationshipUpdate{
		Operation: op,
		Relationship: &authzed.Relationship{
			Resource: &authzed.ObjectReference{ObjectType: string(rel.ResourceType), ObjectId: rel.ResourceID},
			Relation: rel.Relation,
			Subject:  &authzed.SubjectReference{Object: &authzed.ObjectReference{ObjectType: string(subjectType), ObjectId: rel.SubjectID}},
		},
	}
}

func mustStruct(m map[string]any) *structpb.Struct {
	s, err := structpb.NewStruct(m)
	if err != nil {
		panic(err)
	}
	return s
}
