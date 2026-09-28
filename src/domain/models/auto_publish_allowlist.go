package models

import (
	"time"

	"github.com/uptrace/bun"
)

// AutoPublishAllowlistEntry is one platform entry in the workspace's
// auto-publish allowlist. PlatformID stores Zernio's wire
// identifier (e.g. "linkedin"), matching POST
// /api/integrations/zernio/connect-links semantics.
type AutoPublishAllowlistEntry struct {
	bun.BaseModel `bun:"table:auto_publish_allowlist,alias:ap" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks

	PlatformID string    `bun:"platform_id,pk" json:"platform_id"`
	CreatedAt  time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}
