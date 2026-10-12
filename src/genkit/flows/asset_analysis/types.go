package asset_analysis

import (
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

// SourceKind says how the source should be read.
type SourceKind string

const (
	// SourceKindDocument is written text: a document, page or markdown note.
	SourceKindDocument SourceKind = "document"
	// SourceKindRecording is a transcript, usually a meeting; the analysis adds
	// participants, commitments, decisions and open questions.
	SourceKindRecording SourceKind = "recording"
)

// Segment is one citable piece of the source, in reading order.
type Segment struct {
	// Label is how the model cites the segment, e.g. "p. 12", "Slide 4" or
	// "12:03–12:47". Empty for sources without anchors.
	Label string
	Text  string
	// Tokens is the source-reported token count; zero is estimated.
	Tokens int
}

// Source is the asset text to analyse.
type Source struct {
	AssetID  string
	Title    string
	Kind     SourceKind
	Segments []Segment
}

// CampaignBrief is the campaign a campaign lens reads the asset through.
type CampaignBrief struct {
	Name           string
	Description    string
	TargetPersona  string
	KeyMessages    string
	ToneGuidelines string
	Language       string
	CampaignType   string
	// Phases are the campaign type's phase names in order.
	Phases []string
	// Goal is the campaign's posting goal in words, empty when unset.
	Goal string
}

// SiblingSummary is another campaign asset's workspace summary.
type SiblingSummary struct {
	Title   string
	Summary string
}

// Lens is the context an analysis is made in. Campaign is nil for the
// workspace lens.
type Lens struct {
	WorkspaceName string
	// BrandBlock is the rendered brand voice, audience, guardrails and current
	// facts.
	BrandBlock string
	Campaign   *CampaignBrief
	Siblings   []SiblingSummary
	// ExistingIdeas are idea titles already in scope, so the model avoids
	// repeating them.
	ExistingIdeas []string
	PostTitles    []string
}

// AnalyzeRequest is the input to the flow.
type AnalyzeRequest struct {
	// CampaignID only labels logs; the lens content is in Lens.
	CampaignID string
	Source     Source
	Lens       Lens
}

// Entity is a named thing the source mentions.
type Entity struct {
	Name string `json:"name" jsonschema:"description=The name as written in the source"`
	Kind string `json:"kind" jsonschema:"description=What it is,enum=person,enum=org,enum=product,enum=place,enum=other"`
}

// Participant is a person who took part in a recording.
type Participant struct {
	Name       string `json:"name"       jsonschema:"description=The name as spoken; never invented"`
	Role       string `json:"role"       jsonschema:"description=Role or company when said, else empty"`
	Confidence string `json:"confidence" jsonschema:"description=stated when the person was named or introduced, inferred otherwise,enum=stated,enum=inferred"`
}

// Commitment is a promise made in a recording.
type Commitment struct {
	Owner       string `json:"owner"       jsonschema:"description=Who promised it, or Unattributed"`
	Commitment  string `json:"commitment"  jsonschema:"description=What was promised"`
	Due         string `json:"due"         jsonschema:"description=When, as said, else empty"`
	SourceLabel string `json:"sourceLabel" jsonschema:"description=Label of the segment it was said in"`
	Confidence  string `json:"confidence"  jsonschema:"description=stated or inferred,enum=stated,enum=inferred"`
}

// Decision is a decision taken in a recording.
type Decision struct {
	Text        string `json:"text"        jsonschema:"description=The decision"`
	SourceLabel string `json:"sourceLabel" jsonschema:"description=Label of the segment it was taken in"`
}

// Relevance scores how well the asset serves the campaign.
type Relevance struct {
	Score int    `json:"score" jsonschema:"description=0 to 100"`
	Why   string `json:"why"   jsonschema:"description=One sentence"`
}

// Idea is a content idea distilled from the source.
type Idea struct {
	Title        string   `json:"title"        jsonschema:"description=A post idea in at most 120 characters"`
	Angle        string   `json:"angle"        jsonschema:"description=The hook or angle that makes it a post"`
	Rationale    string   `json:"rationale"    jsonschema:"description=Why it fits the workspace or campaign"`
	SourceLabels []string `json:"sourceLabels" jsonschema:"description=Labels of the segments it draws on"`
}

// Analysis is the model's structured output after server-side clean-up.
// Field order is the order the prompt asks for: ideas sit right after the
// summary so a truncated reply still carries them.
type Analysis struct {
	Language      string        `json:"language"      jsonschema:"description=ISO 639-1 code of the language the analysis is written in"`
	Summary       string        `json:"summary"       jsonschema:"description=150-400 word markdown summary"`
	Ideas         []Idea        `json:"ideas"         jsonschema:"description=5 to 10 post ideas"`
	KeyPoints     []string      `json:"keyPoints"     jsonschema:"description=Up to 10 key points"`
	Topics        []string      `json:"topics"        jsonschema:"description=Up to 8 topics"`
	Entities      []Entity      `json:"entities"      jsonschema:"description=People, organisations, products and places mentioned"`
	Participants  []Participant `json:"participants"  jsonschema:"description=Recordings only"`
	Commitments   []Commitment  `json:"commitments"   jsonschema:"description=Recordings only"`
	Decisions     []Decision    `json:"decisions"     jsonschema:"description=Recordings only"`
	OpenQuestions []string      `json:"openQuestions" jsonschema:"description=Recordings only"`
	Relevance     *Relevance    `json:"relevance"     jsonschema:"description=Campaign lens only"`
}

// Strategy is how the source was fed to the model.
type Strategy string

const (
	StrategyFull      Strategy = "full"
	StrategyMapReduce Strategy = "map_reduce"
)

// AnalyzeResponse is the flow's result.
type AnalyzeResponse struct {
	Analysis Analysis
	Strategy Strategy
	// Truncated is set when the source exceeded MaxSourceTokens and its tail
	// was not read.
	Truncated bool
	// SourceTokens is the (estimated) size of the source that was read.
	SourceTokens int
	// ModelID is the analyze-slot model; MapModelID the map-slot model of a
	// map-reduce run.
	ModelID    string
	MapModelID string
}

// FlowConfig holds static settings.
type FlowConfig struct {
	// Provider builds the Anthropic call config.
	Provider *llm.Provider
	// Recorder captures usage events; nil disables recording.
	Recorder *usage.Recorder
	// Checker gates the flow against the tenant's spend caps; nil = no gate.
	Checker *usage.Checker
	// FullContextMaxTokens is the largest source analysed in one call; a larger
	// one goes through map-reduce. 0 falls back to 150000.
	FullContextMaxTokens int
	// MaxSourceTokens caps how much of the source is read at all. 0 falls back
	// to 1000000.
	MaxSourceTokens int
	// MapGroupTokens sizes one map call's share of the source. 0 falls back to
	// 30000.
	MapGroupTokens int
	// MaxParallelMaps caps concurrent map calls. 0 falls back to 4.
	MaxParallelMaps int
	// MaxOutputTokens caps one model call. 0 falls back to 32768.
	MaxOutputTokens int64
}

// ValidationError means the request cannot be analysed (empty source).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// AIError means a model call failed or returned nothing usable.
type AIError struct {
	Msg string
	// Invalid is set when the model answered but the answer was not valid
	// JSON even after a repair attempt, so retrying the job will not help.
	Invalid bool
	// Refused is set when the model declined the request. A refusal is never
	// retried: the same source would most likely be refused again.
	Refused bool
}

func (e *AIError) Error() string { return e.Msg }
