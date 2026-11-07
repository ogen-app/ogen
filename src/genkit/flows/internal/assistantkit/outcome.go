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
