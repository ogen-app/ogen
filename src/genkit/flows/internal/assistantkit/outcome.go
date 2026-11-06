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
)

// Option is one way forward from a conflict.
type Option struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
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
