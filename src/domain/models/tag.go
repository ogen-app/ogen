package models

import (
	"time"

	"github.com/uptrace/bun"
)

type Tag struct {
	bun.BaseModel `bun:"table:tags,alias:t" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks

	ID        string    `bun:"id,pk"                                        json:"id"`
	Name      string    `bun:"name,notnull"                                 json:"name"`
	Color     string    `bun:"color,notnull"                                json:"color"`
	CreatedBy string    `bun:"created_by,notnull"                           json:"-"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"-"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"-"`
}
