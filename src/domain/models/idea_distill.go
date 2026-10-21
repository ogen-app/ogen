package models

import (
	"time"

	"github.com/uptrace/bun"
)

// IdeaDistillStatus is where a distill run is in its lifecycle. It is a closed
// set mirrored by a DB CHECK.
type IdeaDistillStatus string

const (
	IdeaDistillStatusPending IdeaDistillStatus = "pending"
	IdeaDistillStatusRunning IdeaDistillStatus = "running"
	IdeaDistillStatusReady   IdeaDistillStatus = "ready"
	IdeaDistillStatusFailed  IdeaDistillStatus = "failed"
	// IdeaDistillStatusSkipped means the run was gated (entitlement off or
	// monthly cap reached); FailureCode says which.
	IdeaDistillStatusSkipped IdeaDistillStatus = "skipped"
)

// Active reports whether the run is still waiting or running.
func (s IdeaDistillStatus) Active() bool {
	return s == IdeaDistillStatusPending || s == IdeaDistillStatusRunning
}

// Bounds of a distill request.
const (
	IdeaDistillMinCount     = 3
	IdeaDistillMaxCount     = 10
	IdeaDistillDefaultCount = 5
	IdeaDistillMaxFocus     = 300
)

// AnalysisCodeLensGone means the lens a run was made for went away before
// its ideas were saved.
const AnalysisCodeLensGone = "lens_gone"

// IdeaDistillRun is one request for more ideas from an analysis lens.
type IdeaDistillRun struct {
	bun.BaseModel `bun:"table:idea_distill_runs,alias:idr" swaggerignore:"true"`
	TenantScoped

	ID         string            `bun:"id,pk"                       json:"id"`
	AnalysisID string            `bun:"analysis_id,notnull"         json:"analysis_id"`
	AssetID    string            `bun:"asset_id,notnull"            json:"asset_id"`
	CampaignID *string           `bun:"campaign_id"                 json:"campaign_id"  extensions:"x-nullable"`
	Status     IdeaDistillStatus `bun:"status,notnull"              json:"status"       swaggertype:"string" enums:"pending,running,ready,failed,skipped"`
	Focus      string            `bun:"focus,notnull,default:''"    json:"focus"`
	Count      int               `bun:"requested_count,notnull"     json:"count"`
	IdeasCount int               `bun:"ideas_count,notnull,default:0" json:"ideas_count"`
	// ModelID is the model that ran the distill. Spend lives in the analytics
	// usage events, not here.
	ModelID       string     `bun:"model_id,notnull,default:''"                  json:"-"`
	FailureCode   string     `bun:"failure_code,notnull,default:''"              json:"failure_code"`
	FailureReason string     `bun:"failure_reason,notnull,default:''"            json:"failure_reason"`
	CreatedBy     *string    `bun:"created_by"                                   json:"created_by" extensions:"x-nullable"`
	CreatedAt     time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt     time.Time  `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
	FinishedAt    *time.Time `bun:"finished_at"                                  json:"finished_at" extensions:"x-nullable"`
}

// IdeaDistillCharge records one distill run that reached the model, for the
// monthly cap.
type IdeaDistillCharge struct {
	bun.BaseModel `bun:"table:idea_distill_ledger,alias:idl" swaggerignore:"true"`
	TenantScoped

	ID        string    `bun:"id,pk"`
	RunID     string    `bun:"run_id,notnull"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp"`
}
