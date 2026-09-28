package models

import (
	"time"

	"github.com/uptrace/bun"
)

// TenantGroupAssignment is the join row realising the many-to-many relation
// between tenants and tenant_groups: a tenant belongs to many groups
// and a group holds many tenants. The (tenant_id, group_id) pair is the primary
// key, so a tenant is at most once in a given group (assignment is idempotent).
//
// Global operator table — NOT TenantScoped. Rows cascade when either the tenant
// or the group is deleted.
type TenantGroupAssignment struct {
	bun.BaseModel `bun:"table:tenant_group_assignments,alias:tga" swaggerignore:"true"`

	TenantID  string    `bun:"tenant_id,pk"                                 json:"tenant_id"`
	GroupID   string    `bun:"group_id,pk"                                  json:"group_id"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}
