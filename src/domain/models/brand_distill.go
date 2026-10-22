package models

import (
	"time"

	"github.com/uptrace/bun"
)

// BrandDistillStatus is where a brand distillation run is. It is a closed set
// mirrored by a DB CHECK.
type BrandDistillStatus string

const (
	BrandDistillStatusPending BrandDistillStatus = "pending"
	// BrandDistillStatusCollecting: pages are being imported, or the source
	// assets are still being ingested.
	BrandDistillStatusCollecting BrandDistillStatus = "collecting"
	// BrandDistillStatusReading: the sources are read for evidence.
	BrandDistillStatusReading BrandDistillStatus = "reading"
	// BrandDistillStatusComposing: the proposal is being written.
	BrandDistillStatusComposing BrandDistillStatus = "composing"
	// BrandDistillStatusReady: a proposal waits for review.
	BrandDistillStatusReady BrandDistillStatus = "ready"
	// BrandDistillStatusCommitted: the kept items were saved.
	BrandDistillStatusCommitted BrandDistillStatus = "committed"
	// BrandDistillStatusSuperseded: answers made a newer run (ChildRunID).
	BrandDistillStatusSuperseded BrandDistillStatus = "superseded"
	BrandDistillStatusDiscarded  BrandDistillStatus = "discarded"
	// BrandDistillStatusExpired: a proposal nobody committed in time; its
	// working state is gone.
	BrandDistillStatusExpired BrandDistillStatus = "expired"
	BrandDistillStatusFailed  BrandDistillStatus = "failed"
)

// Open reports whether the run is still being worked on.
func (s BrandDistillStatus) Open() bool {
	switch s {
	case BrandDistillStatusPending, BrandDistillStatusCollecting, BrandDistillStatusReading, BrandDistillStatusComposing:
		return true
	}
	return false
}

// BrandDistillOpenStatuses are the statuses of a run still being worked on.
var BrandDistillOpenStatuses = []BrandDistillStatus{
	BrandDistillStatusPending, BrandDistillStatusCollecting, BrandDistillStatusReading, BrandDistillStatusComposing,
}

// Failure codes of a brand distillation run.
const (
	BrandDistillCodeNoSources      = "no_usable_sources"
	BrandDistillCodeScraping       = "scraping_unavailable"
	BrandDistillCodeModelRefused   = "model_refused"
	BrandDistillCodeInvalidOutput  = "invalid_output"
	BrandDistillCodeInternal       = "internal"
	BrandDistillCodeTenantInactive = "tenant_inactive"
	BrandDistillCodeDisabled       = "entitlement_disabled"
	BrandDistillCodeLimitReached   = "limit_reached"
)

// Statuses of one source of a run.
const (
	DistillSourcePending      = "pending"
	DistillSourceReady        = "ready"
	DistillSourcePartial      = "partial"
	DistillSourceFailed       = "failed"
	DistillSourceTimedOut     = "timed_out"
	DistillSourceSkippedQuota = "skipped_quota"
	// DistillSourceThin is a page that came back with too little text to read.
	DistillSourceThin = "thin"
)

// Roles of a source: imported by the website read, or chosen by the user.
const (
	DistillRoleWebsite = "website"
	DistillRoleAsset   = "asset"
)

// DistillWebsite is what a run's website read found.
type DistillWebsite struct {
	URL        string `json:"url"`
	Mapped     bool   `json:"mapped"`
	PagesFound int    `json:"pagesFound"`
}

// DistillSource is one asset a run reads.
type DistillSource struct {
	AssetID   string `json:"assetId"`
	Title     string `json:"title"`
	Type      string `json:"type"`
	Role      string `json:"role"`
	Status    string `json:"status"`
	URL       string `json:"url,omitempty"`
	Truncated bool   `json:"truncated"`
}

// DistillAnswer is the user's answer to one question.
type DistillAnswer struct {
	QuestionID string `json:"questionId"`
	Question   string `json:"question"`
	Answer     string `json:"answer"`
}

// DistillEvidence is where a proposed item came from: a quote from a source
// segment, or an answer.
type DistillEvidence struct {
	// Kind is "answer" for an answer, empty for a quote.
	Kind       string `json:"kind,omitempty"`
	QuestionID string `json:"questionId,omitempty"`
	// Ref is "<assetId>#<segment>".
	Ref     string `json:"ref,omitempty"`
	AssetID string `json:"assetId,omitempty"`
	Label   string `json:"label,omitempty"`
	Quote   string `json:"quote,omitempty"`
}

// EvidenceKindAnswer marks evidence that is an answer.
const EvidenceKindAnswer = "answer"

// DistillCandidate is one passage the reading stage kept as evidence. AssetID
// and Label are filled from the segment it was quoted from.
type DistillCandidate struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref"`
	Quote   string `json:"quote"`
	Note    string `json:"note,omitempty"`
	AssetID string `json:"assetId,omitempty"`
	Label   string `json:"label,omitempty"`
}

// ProposedSample is a voice sample: a passage quoted word for word.
type ProposedSample struct {
	Text     string          `json:"text"`
	Evidence DistillEvidence `json:"evidence"`
}

// ProposedVoice is a voice a run proposes.
type ProposedVoice struct {
	ProposalID   string            `json:"proposalId"`
	Name         string            `json:"name"`
	WhenToUse    string            `json:"whenToUse"`
	IsDefault    bool              `json:"isDefault"`
	Thin         bool              `json:"thin"`
	Samples      []ProposedSample  `json:"samples"`
	Rules        VoiceRules        `json:"rules"`
	ChannelNotes map[string]string `json:"channelNotes"`
	Evidence     []DistillEvidence `json:"evidence"`
}

// ProposedAudience is an audience a run proposes.
type ProposedAudience struct {
	ProposalID      string            `json:"proposalId"`
	Name            string            `json:"name"`
	Who             string            `json:"who"`
	ReadsOn         string            `json:"readsOn"`
	ScrollsPastWhen string            `json:"scrollsPastWhen"`
	BelievesWhen    string            `json:"believesWhen"`
	Evidence        []DistillEvidence `json:"evidence"`
}

// ProposedLine is one guardrail line or banned word.
type ProposedLine struct {
	ProposalID string            `json:"proposalId"`
	Text       string            `json:"text"`
	Evidence   []DistillEvidence `json:"evidence"`
}

// ProposedDisclaimer is a disclaimer the sources run; Current is the
// workspace's own, nil when it has none.
type ProposedDisclaimer struct {
	ProposalID string            `json:"proposalId"`
	Text       string            `json:"text"`
	Current    *string           `json:"current"`
	Evidence   []DistillEvidence `json:"evidence"`
}

// ProposedGuardrails are the guardrail additions a run proposes.
type ProposedGuardrails struct {
	MayClaim    []ProposedLine      `json:"mayClaim"`
	NeverClaim  []ProposedLine      `json:"neverClaim"`
	BannedWords []ProposedLine      `json:"bannedWords"`
	Disclaimer  *ProposedDisclaimer `json:"disclaimer"`
}

// ProposedFact is a ledger fact a run proposes.
type ProposedFact struct {
	ProposalID string            `json:"proposalId"`
	Statement  string            `json:"statement"`
	Subject    FactSubject       `json:"subject"`
	Kind       FactKind          `json:"kind"`
	ExpiresAt  *CalendarDate     `json:"expiresAt" swaggertype:"string"`
	Source     string            `json:"source"`
	Evidence   []DistillEvidence `json:"evidence"`
}

// DistillQuestion is a question about something the sources left out, or a
// starter question.
type DistillQuestion struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Section string `json:"section"`
	Why     string `json:"why,omitempty"`
}

// DistillCoverage says how much a section had to go on.
type DistillCoverage struct {
	// Level is found, thin or none.
	Level string `json:"level"`
	Note  string `json:"note"`
}

// DistillDropped counts the proposed items left out, by reason.
type DistillDropped struct {
	Ungrounded int `json:"ungrounded"`
	Duplicate  int `json:"duplicate"`
	Limit      int `json:"limit"`
}

// BrandProposal is what a run proposes. Nothing in it is brand material until
// it is committed.
type BrandProposal struct {
	Voices     []ProposedVoice            `json:"voices"`
	Audiences  []ProposedAudience         `json:"audiences"`
	Guardrails ProposedGuardrails         `json:"guardrails"`
	Facts      []ProposedFact             `json:"facts"`
	Questions  []DistillQuestion          `json:"questions"`
	Coverage   map[string]DistillCoverage `json:"coverage"`
	Dropped    DistillDropped             `json:"dropped"`
}

// BrandDistillRun is one brand distillation.
type BrandDistillRun struct {
	bun.BaseModel `bun:"table:brand_distill_runs,alias:bdr" swaggerignore:"true"`
	TenantScoped

	ID          string             `bun:"id,pk"                              json:"id"`
	ParentRunID *string            `bun:"parent_run_id"                      json:"parentRunId" extensions:"x-nullable"`
	ChildRunID  *string            `bun:"child_run_id"                       json:"childRunId"  extensions:"x-nullable"`
	Depth       int                `bun:"depth,notnull,default:0"            json:"-"`
	Status      BrandDistillStatus `bun:"status,notnull"                     json:"status" swaggertype:"string" enums:"pending,collecting,reading,composing,ready,committed,superseded,discarded,expired,failed"`
	WebsiteURL  *string            `bun:"website_url"                        json:"-"`
	Website     *DistillWebsite    `bun:"website,type:jsonb"                 json:"website"`
	Sources     []DistillSource    `bun:"sources,notnull,type:jsonb"         json:"sources"`
	Answers     []DistillAnswer    `bun:"answers,notnull,type:jsonb"         json:"answers"`
	// Candidates are the evidence the reading stage kept; a refine composes
	// from them without reading the sources again.
	Candidates []DistillCandidate `bun:"candidates,type:jsonb"              json:"-"`
	Proposal   *BrandProposal     `bun:"proposal,type:jsonb"                json:"proposal"`
	// ModelIDs are the models of each stage, frozen for the run.
	ModelIDs StringMap `bun:"model_ids,notnull,type:jsonb"       json:"-"`
	// Charged is set once the run counts against the monthly allowance.
	Charged       bool       `bun:"charged,notnull,default:false"      json:"-"`
	FailureCode   string     `bun:"failure_code,notnull,default:''"    json:"-"`
	FailureReason string     `bun:"failure_reason,notnull,default:''"  json:"-"`
	CreatedBy     *string    `bun:"created_by"                         json:"createdBy" extensions:"x-nullable"`
	CommittedBy   *string    `bun:"committed_by"                       json:"-"`
	CommittedAt   *time.Time `bun:"committed_at"                       json:"committedAt" extensions:"x-nullable"`
	// CommittedIDs maps each committed proposalId to what it became.
	CommittedIDs StringMap  `bun:"committed_ids,notnull,type:jsonb"             json:"-"`
	CreatedAt    time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"createdAt"`
	UpdatedAt    time.Time  `bun:"updated_at,notnull,default:current_timestamp" json:"updatedAt"`
	FinishedAt   *time.Time `bun:"finished_at"                                  json:"finishedAt" extensions:"x-nullable"`
}

// BrandDistillCharge records one run against the monthly cap.
type BrandDistillCharge struct {
	bun.BaseModel `bun:"table:brand_distill_ledger,alias:bdl" swaggerignore:"true"`
	TenantScoped

	ID        string    `bun:"id,pk"`
	RunID     string    `bun:"run_id,notnull"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp"`
}
