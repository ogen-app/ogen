package models

import (
	"time"

	"github.com/uptrace/bun"
)

// TenantGroup is an operator-owned, reusable classification (cohort / segment)
// that many tenants can belong to. The tenant↔group relation is
// many-to-many (see TenantGroupAssignment). Groups are managed by Harbor over
// the internal gRPC surface (src/grpc/server).
//
// Like Tenant, this is a GLOBAL table — NOT TenantScoped: it is read and
// written cross-tenant by operators, so it carries no tenant_id.
type TenantGroup struct {
	bun.BaseModel `bun:"table:tenant_groups,alias:tg" swaggerignore:"true"`

	ID          string    `bun:"id,pk"                                        json:"id"`
	Name        string    `bun:"name,notnull,unique"                          json:"name"`
	Color       string    `bun:"color,notnull,default:''"                     json:"color"`
	Description string    `bun:"description,notnull,default:''"                json:"description"`
	CreatedAt   time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt   time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}
