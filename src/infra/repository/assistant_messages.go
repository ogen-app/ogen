package repository

import (
	"context"
	"slices"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// AssistantMessageRepository stores universal-assistant threads, one per
// scope.
type AssistantMessageRepository interface {
	// CreateBatch inserts a turn's messages all-or-nothing, so a turn never
	// persists half-written.
	CreateBatch(ctx context.Context, msgs []*models.AssistantMessage) error
	// ListRecent returns a thread's last limit messages, oldest first.
	ListRecent(ctx context.Context, kind models.AssistantScopeKind, scopeID string, limit int) ([]models.AssistantMessage, error)
	// ListThreads returns up to limit of the workspace's threads, latest
	// message first, from those whose latest message is before before (zero
	// for no bound).
	ListThreads(ctx context.Context, before time.Time, limit int) ([]AssistantThread, error)
}

// AssistantThread is one thread in the workspace's list: its scope, the
// labels of its subject, and its latest message. A campaign or post thread
// whose campaign is deleted isn't listed, and neither is a post thread whose
// post is gone.
type AssistantThread struct {
	ScopeKind models.AssistantScopeKind `bun:"scope_kind"`
	ScopeID   string                    `bun:"scope_id"`
	// Title is the campaign's name or the post's title; empty for the
	// workspace.
	Title string `bun:"title"`
	// CampaignID and CampaignTitle are the campaign's own for a campaign
	// thread and the post's campaign's for a post thread.
	CampaignID    string `bun:"campaign_id"`
	CampaignTitle string `bun:"campaign_title"`
	// Archived reports that the thread's campaign is archived.
	Archived      bool      `bun:"archived"`
	LastMessageAt time.Time `bun:"last_message_at"`
	LastRole      string    `bun:"last_role"`
	LastContent   string    `bun:"last_content"`
	MessageCount  int       `bun:"message_count"`
}

type assistantMessageRepository struct {
	db *bun.DB
}

// NewAssistantMessageRepository returns the bun-backed AssistantMessageRepository.
func NewAssistantMessageRepository(db *bun.DB) AssistantMessageRepository {
	return &assistantMessageRepository{db: db}
}

// CreateBatch inserts msgs in one statement inside a transaction, in slice
// order, so seq follows the batch order. A post thread's messages are stored
// only while the post exists: its row is held until they commit, so a post
// deleted meanwhile takes them with it, and one already gone stores none.
func (r *assistantMessageRepository) CreateBatch(ctx context.Context, msgs []*models.AssistantMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if m := msgs[0]; m.ScopeKind == models.AssistantScopePost {
			var ids []string
			if err := tx.NewSelect().Model((*models.Post)(nil)).Column("po.id").
				Where("po.id = ?", m.ScopeID).For("KEY SHARE").Scan(ctx, &ids); err != nil {
				return err
			}
			if len(ids) == 0 {
				return nil
			}
		}
		_, err := tx.NewInsert().Model(&msgs).Exec(ctx)
		return err
	})
}

// ListRecent returns the thread's last limit messages, oldest first, and none
// for a limit below one. Messages that share created_at keep insertion order.
func (r *assistantMessageRepository) ListRecent(ctx context.Context, kind models.AssistantScopeKind, scopeID string, limit int) ([]models.AssistantMessage, error) {
	if limit <= 0 {
		// bun drops a zero LIMIT, which would load the whole thread.
		return nil, nil
	}
	var msgs []models.AssistantMessage
	err := r.db.NewSelect().Model(&msgs).
		Where("asm.scope_kind = ? AND asm.scope_id = ?", kind, scopeID).
		// seq breaks created_at ties in insertion order.
		OrderExpr("asm.created_at DESC, asm.seq DESC").
		Limit(limit).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	slices.Reverse(msgs)
	return msgs, nil
}

// listThreadsSQL takes each thread's latest message off the thread index,
// counts the thread on the way, and joins its subject for the labels. A raw
// query bypasses the TenantScoped hooks, so every table is scoped by the
// tenant argument. Threads tied on their latest message order by scope, so
// the order is stable.
const listThreadsSQL = `
SELECT t.scope_kind, t.scope_id,
       CASE t.scope_kind WHEN 'campaign' THEN c.name WHEN 'post' THEN p.title ELSE '' END AS title,
       COALESCE(c.id, pc.id, '') AS campaign_id,
       COALESCE(c.name, pc.name, '') AS campaign_title,
       COALESCE(c.archived_at, pc.archived_at) IS NOT NULL AS archived,
       t.created_at AS last_message_at, t.role AS last_role, t.content AS last_content,
       t.message_count
FROM (
    SELECT DISTINCT ON (asm.scope_kind, asm.scope_id)
           asm.scope_kind, asm.scope_id, asm.role, asm.content, asm.created_at,
           count(*) OVER (PARTITION BY asm.scope_kind, asm.scope_id) AS message_count
    FROM assistant_messages AS asm
    WHERE asm.tenant_id = ?0
    ORDER BY asm.scope_kind, asm.scope_id, asm.created_at DESC, asm.seq DESC
) AS t
LEFT JOIN campaigns AS c
       ON t.scope_kind = 'campaign' AND c.id = t.scope_id AND c.tenant_id = ?0
LEFT JOIN posts AS p
       ON t.scope_kind = 'post' AND p.id = t.scope_id AND p.tenant_id = ?0
LEFT JOIN campaigns AS pc
       ON pc.id = p.campaign_id AND pc.tenant_id = ?0
WHERE (t.scope_kind = 'workspace'
       OR (t.scope_kind = 'campaign' AND c.id IS NOT NULL AND c.deleted_at IS NULL)
       OR (t.scope_kind = 'post' AND pc.id IS NOT NULL AND pc.deleted_at IS NULL))
  AND (?1::timestamptz IS NULL OR t.created_at < ?1)
ORDER BY t.created_at DESC, t.scope_kind, t.scope_id
LIMIT ?2`

// ListThreads returns the workspace's threads, latest message first, and none
// for a limit below one.
func (r *assistantMessageRepository) ListThreads(ctx context.Context, before time.Time, limit int) ([]AssistantThread, error) {
	if limit <= 0 {
		return nil, nil
	}
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return nil, tenantctx.ErrNoTenant
	}
	var bound *time.Time
	if !before.IsZero() {
		bound = &before
	}
	var threads []AssistantThread
	if err := r.db.NewRaw(listThreadsSQL, tid, bound, limit).Scan(ctx, &threads); err != nil {
		return nil, err
	}
	return threads, nil
}
