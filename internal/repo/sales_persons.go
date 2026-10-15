package repo

import (
	"context"
	"strings"

	"b2b_pilot_v3/internal/bridge"
)

// ListSalesPersons returns local demo sales users for a tenant.
// The WeCom user id is exposed as OpenID in local mode so the existing UI
// contract remains stable; the real Feishu bridge supplies its own OpenID.
func (r *Repo) ListSalesPersons(ctx context.Context, tenantID string) ([]bridge.SalesPerson, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, role, COALESCE(wecom_user_id, '')
		FROM users
		WHERE tenant_id = $1 AND role = 'SALES'
		ORDER BY name, id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]bridge.SalesPerson, 0)
	for rows.Next() {
		var item bridge.SalesPerson
		var role string
		if err := rows.Scan(&item.UserID, &item.Name, &role, &item.OpenID); err != nil {
			return nil, err
		}
		item.RoleKey = strings.ToLower(role)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}
