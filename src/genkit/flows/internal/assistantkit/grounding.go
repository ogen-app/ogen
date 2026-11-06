package assistantkit

// GroundingMode says whether generated content came from the user's own
// material.
type GroundingMode string

const (
	// Grounded content drew on the listed sources.
	Grounded GroundingMode = "grounded"
	// Exploratory content came from general knowledge; the UI labels it
	// "not grounded in your sources".
	Exploratory GroundingMode = "exploratory"
)

// Source is one piece of the user's material generation drew on.
type Source struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Grounding is attached to every generating tool's result, so the UI can
// badge exploratory output without relying on how the model words its reply.
type Grounding struct {
	Mode    GroundingMode `json:"mode"`
	Sources []Source      `json:"sources,omitempty"`
}

// GroundingFrom is grounded when any source informed the output.
func GroundingFrom(sources []Source) Grounding {
	if len(sources) == 0 {
		return Grounding{Mode: Exploratory}
	}
	return Grounding{Mode: Grounded, Sources: sources}
}
