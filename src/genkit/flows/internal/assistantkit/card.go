package assistantkit

// Card is what an offer's card shows beyond its summary: the button that
// confirms it, what it draws on and what it will make. Every field is
// optional; a card without one shows the summary and a plain confirm.
type Card struct {
	// Verb names the confirm button, e.g. "Create campaign", "Draft 5 posts".
	Verb string `json:"verb,omitempty"`
	// Meta follows the headline, e.g. "from this brochure", "from 4 assets".
	Meta string `json:"meta,omitempty"`
	// Rows are the offer's details, in order.
	Rows []CardRow `json:"rows,omitempty"`
	// Sources are the assets the work draws on.
	Sources []CardSource `json:"sources,omitempty"`
	// Footnote closes the card, e.g. what happens after the yes.
	Footnote string `json:"footnote,omitempty"`
	// Widen offers the same work over more sources than the narrow default
	// it was offered on, e.g. the campaign's assets instead of the open one.
	Widen *CardWiden `json:"widen,omitempty"`
}

// The kinds of card row.
const (
	// CardRowDestination is where the work lands: a campaign, platforms.
	CardRowDestination = "destination"
	// CardRowType is the kind of thing made, e.g. a campaign type.
	CardRowType = "type"
	// CardRowDates is when, with Start and End and the reason in Note.
	CardRowDates = "dates"
	// CardRowGrounding is what the work is grounded in.
	CardRowGrounding = "grounding"
	// CardRowTags is the tags added or removed.
	CardRowTags = "tags"
)

// CardRow is one detail of an offer. Kind lets the client label and lay it
// out; Value is the detail as text.
type CardRow struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
	// Note explains the value, e.g. why those dates.
	Note string `json:"note,omitempty"`
	// Start and End are a dates row's days, YYYY-MM-DD, for the client to
	// format.
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
}

// CardSource is one asset an offer draws on. Type is the asset's kind, for
// its badge: PDF, DOC, IMG, AUDIO, URL or MD.
type CardSource struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Type  string `json:"type,omitempty"`
}

// CardWiden is a wider source the card offers. Label is the button, e.g.
// "Use the campaign's 6 assets"; Instruction is the turn the client sends
// when it is pressed.
type CardWiden struct {
	Note        string `json:"note"`
	Label       string `json:"label"`
	Instruction string `json:"instruction"`
}
