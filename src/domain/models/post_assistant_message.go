package models

import (
	"time"

	"github.com/uptrace/bun"
)

type PostAssistantMessage struct {
	bun.BaseModel `bun:"table:post_assistant_messages,alias:pam" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks

	ID        string    `bun:"id,pk"                                        json:"id"`
	PostID    string    `bun:"post_id,notnull"                              json:"post_id"`
	Role      string    `bun:"role,notnull"                                 json:"role"`
	Content   string    `bun:"content,notnull"                              json:"content"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}
