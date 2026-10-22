// Package brand_distill reads a brand's own material and proposes voices,
// audiences, guardrails and facts from it. It is two stages: extract reads
// each part of the sources for passages that show the brand, quoted word for
// word; compose writes the proposal from those passages, the user's answers
// and what the workspace already has. Grounding the proposal against the
// passages is the caller's job.
package brand_distill

import (
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

// Candidate kinds: what a passage is evidence of.
const (
	KindVoice       = "voice_passage"
	KindAudience    = "audience_signal"
	KindFact        = "fact"
	KindClaim       = "claim"
	KindRestriction = "restriction"
	KindDisclaimer  = "disclaimer"
	KindTerm        = "term"
)

// CandidateKinds are the kinds extract may return.
var CandidateKinds = []string{KindVoice, KindAudience, KindFact, KindClaim, KindRestriction, KindDisclaimer, KindTerm}

// Segment is one citable piece of a source.
type Segment struct {
	// Ref identifies the segment within the run, e.g. "s2.7".
	Ref string
	// Label is how a person would cite it: "acme.com/about", "Deck · Slide 4".
	Label string
	Text  string
	// Tokens is the ingest-reported count; zero is estimated.
	Tokens int
	// Recording is set for a transcript segment.
	Recording bool
}

// ExtractRequest is the sources to read, in reading order.
type ExtractRequest struct {
	Segments []Segment
}

// ExtractResponse is the passages kept, each verified word for word against
// its segment.
type ExtractResponse struct {
	Candidates []models.DistillCandidate
	// Parts is how many calls the reading took.
	Parts   int
	ModelID string
	// Dropped counts passages that were not in their segment as quoted.
	Dropped int
}

// Answer is the user's answer to a question.
type Answer struct {
	QuestionID string
	Question   string
	Answer     string
}

// VoiceDigest and AudienceDigest are what the workspace already has, so the
// proposal does not repeat it.
type VoiceDigest struct {
	Name      string
	WhenToUse string
	Sample    string
}

type AudienceDigest struct {
	Name string
	Who  string
}

// Digest is the workspace's current brand, in brief.
type Digest struct {
	Voices      []VoiceDigest
	Audiences   []AudienceDigest
	MayClaim    []string
	NeverClaim  []string
	BannedWords []string
	Disclaimer  string
	Facts       []string
}

// ComposeRequest is everything the proposal is written from.
type ComposeRequest struct {
	Candidates []models.DistillCandidate
	// Labels maps each candidate ref to its human label.
	Labels  map[string]string
	Answers []Answer
	Digest  Digest
	// Today is YYYY-MM-DD, for reading dated claims.
	Today string
}

// DraftEvidence is a passage (Ref and Quote) or an answer (QuestionID).
type DraftEvidence struct {
	Ref        string `json:"ref"`
	Quote      string `json:"quote"`
	QuestionID string `json:"questionId"`
}

// DraftSample is a voice sample quoted from a passage.
type DraftSample struct {
	Ref  string `json:"ref"`
	Text string `json:"text"`
}

type DraftVoice struct {
	Name         string            `json:"name"`
	WhenToUse    string            `json:"whenToUse"`
	IsDefault    bool              `json:"isDefault"`
	Samples      []DraftSample     `json:"samples"`
	Rules        models.VoiceRules `json:"rules"`
	ChannelNotes map[string]string `json:"channelNotes"`
	Evidence     []DraftEvidence   `json:"evidence"`
}

type DraftAudience struct {
	Name            string          `json:"name"`
	Who             string          `json:"who"`
	ReadsOn         string          `json:"readsOn"`
	ScrollsPastWhen string          `json:"scrollsPastWhen"`
	BelievesWhen    string          `json:"believesWhen"`
	Evidence        []DraftEvidence `json:"evidence"`
}

type DraftLine struct {
	Text     string          `json:"text"`
	Evidence []DraftEvidence `json:"evidence"`
}

type DraftDisclaimer struct {
	Ref  string `json:"ref"`
	Text string `json:"text"`
}

type DraftGuardrails struct {
	MayClaim    []DraftLine      `json:"mayClaim"`
	NeverClaim  []DraftLine      `json:"neverClaim"`
	BannedWords []DraftLine      `json:"bannedWords"`
	Disclaimer  *DraftDisclaimer `json:"disclaimer"`
}

type DraftFact struct {
	Statement string          `json:"statement"`
	Subject   string          `json:"subject"`
	Kind      string          `json:"kind"`
	ExpiresAt string          `json:"expiresAt"`
	Evidence  []DraftEvidence `json:"evidence"`
}

type DraftQuestion struct {
	Text    string `json:"text"`
	Section string `json:"section"`
	Why     string `json:"why"`
}

// Draft is the proposal as the model wrote it, before grounding.
type Draft struct {
	Language   string                            `json:"language"`
	Voices     []DraftVoice                      `json:"voices"`
	Audiences  []DraftAudience                   `json:"audiences"`
	Guardrails DraftGuardrails                   `json:"guardrails"`
	Facts      []DraftFact                       `json:"facts"`
	Questions  []DraftQuestion                   `json:"questions"`
	Coverage   map[string]models.DistillCoverage `json:"coverage"`
}

// ComposeResponse is the draft and the model that wrote it.
type ComposeResponse struct {
	Draft   Draft
	ModelID string
}

// FlowConfig holds static settings.
type FlowConfig struct {
	Provider *llm.Provider
	// Recorder captures usage events; nil disables recording.
	Recorder *usage.Recorder
	// Checker gates the flow against the tenant's spend caps; nil = no gate.
	Checker *usage.Checker
	// PartTokens sizes one extract call's share of the sources. 0 is 30000.
	PartTokens int
	// MaxParallelParts caps concurrent extract calls. 0 is 4.
	MaxParallelParts int
}

// ValidationError means there is nothing to read or compose from.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// AIError means a model call failed or returned nothing usable.
type AIError struct {
	Msg string
	// Invalid: the answer was not valid JSON even after a repair turn.
	Invalid bool
	// Refused: the model declined; never retried.
	Refused bool
}

func (e *AIError) Error() string { return e.Msg }
