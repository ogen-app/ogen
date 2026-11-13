package assistantkit

// Status says how a tool call ended.
type Status string

const (
	// StatusOK means the tool did what was asked; Result holds the output.
	StatusOK Status = "ok"
	// StatusRefused means the request cannot be done as asked. Reason says
	// why in plain words; Alternatives lists values that would work.
	StatusRefused Status = "refused"
	// StatusConflict means the request clashes with the current state.
	// Options lists the ways forward the user can pick from.
	StatusConflict Status = "conflict"
	// StatusProposed means the call was held back for the user to confirm.
	// ProposalID names the stored proposal; Summary describes it.
	StatusProposed Status = "proposed"
)

// Option is one way forward from a conflict.
type Option struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	// Recommended marks the way forward the card leads with; an offer has
	// at most one.
	Recommended bool `json:"recommended,omitempty"`
	// Why says in one line why the recommended option is the one to pick;
	// a recommended option must have it.
	Why string `json:"why,omitempty"`
	// Tool and Args are the call picking this option runs. They stay on the
	// server: the model only needs the id to name the user's pick.
	Tool string `json:"-"`
	Args any    `json:"-"`
}

// Outcome is what every assistant tool returns to the model. Expected
// failures travel as a refused or conflict value, never as a Go error: genkit
// aborts the whole generation on a tool error instead of showing it to the
// model.
type Outcome[T any] struct {
	Status       Status   `json:"status"`
	Result       *T       `json:"result,omitempty"`
	Reason       string   `json:"reason,omitempty"`
	Alternatives []string `json:"alternatives,omitempty"`
	Options      []Option `json:"options,omitempty"`
	ProposalID   string   `json:"proposalId,omitempty"`
	Summary      string   `json:"summary,omitempty"`
	// Changes is what changed since a confirmed offer was made, in plain
	// words, when that is why it ran nothing: it was refused, or what still
	// holds was offered afresh (ProposalID).
	Changes []string `json:"changes,omitempty"`
}

// Partial is how much of the job a run did: Made of Total items, the ones
// left undone each with why, and RetryProposalID, an offer of just the
// items worth trying again; it is empty when nothing is. A result embeds
// it, so every reader of the result — the model, a chat turn's results, a
// card's answer — sees it the same.
type Partial struct {
	Made            int       `json:"made"`
	Total           int       `json:"total"`
	NotMade         []NotMade `json:"notMade,omitempty"`
	RetryProposalID string    `json:"retryProposalId,omitempty"`
}

// MarkPartial records how much the run made, what it left undone and the
// retry offered.
func (p *Partial) MarkPartial(made int, notMade []NotMade, retryProposalID string) {
	p.Made, p.Total, p.NotMade, p.RetryProposalID = made, made+len(notMade), notMade, retryProposalID
}

// PartialResult is a result that can say a run did part of the job.
type PartialResult interface {
	MarkPartial(made int, notMade []NotMade, retryProposalID string)
}

// NotMade is an item a run left undone. Platform is the platform it was
// for, when the item is one of several for one subject, e.g. an idea's
// LinkedIn post.
type NotMade struct {
	ID       string `json:"id"`
	Title    string `json:"title,omitempty"`
	Platform string `json:"platform,omitempty"`
	Reason   string `json:"reason"`
}

// OK wraps a successful result.
func OK[T any](v T) Outcome[T] {
	return Outcome[T]{Status: StatusOK, Result: &v}
}

// Refuse reports that the request cannot be done as asked.
func Refuse[T any](reason string, alternatives ...string) Outcome[T] {
	return Outcome[T]{Status: StatusRefused, Reason: reason, Alternatives: alternatives}
}

// Conflict reports that the request clashes with the current state and offers
// ways forward.
func Conflict[T any](reason string, options ...Option) Outcome[T] {
	return Outcome[T]{Status: StatusConflict, Reason: reason, Options: options}
}

// Succeeded reports whether the call did what was asked.
func (o Outcome[T]) Succeeded() bool { return o.Status == StatusOK }

// OutputOK reports whether a tool response's output did not end refused or
// in conflict. Genkit hands the output over JSON-decoded; output that is not
// an outcome counts as ok.
func OutputOK(output any) bool {
	m, ok := output.(map[string]any)
	if !ok {
		return true
	}
	switch s, _ := m["status"].(string); Status(s) {
	case StatusRefused, StatusConflict:
		return false
	}
	return true
}
