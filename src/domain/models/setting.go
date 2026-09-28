package models

import "github.com/uptrace/bun"

type Setting struct {
	bun.BaseModel `bun:"table:settings,alias:st" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks

	Key   string `bun:"key,pk"    json:"key"`
	Value string `bun:"value"     json:"value"`
}
