package assistantkit

// Card is what an offer's card shows beyond its summary: the button that
// confirms it, what it draws on and what it will make. Every field is
// optional; a card without one shows the summary and a plain confirm.
//
// The prose fields (Verb, Meta, Footnote, a row's Value, Widen's Note and
// Label) are English. Each comes with the facts it is worded from, so a
// client words the card in its own language and keeps the prose only for a
// slug it doesn't know. The prose stays for one release, then goes.
type Card struct {
	// Verb names the confirm button, e.g. "Create campaign", "Draft 5 posts".
	Verb string `json:"verb,omitempty"`
	// Action is what the button does, one of the CardAction slugs, and Count
	// how many things it does it to; Verb is the two in words.
	Action string `json:"action,omitempty"`
	Count  int    `json:"count,omitzero"`
	// Item is what Count counts when Action doesn't say, one of the CardItem
	// slugs, e.g. fact for "Remove 6 facts".
	Item string `json:"item,omitempty"`
	// Meta follows the headline, e.g. "from this brochure", "from 4 assets".
	Meta string `json:"meta,omitempty"`
	// DrawsOn is what Meta names, one of the CardDrawsOn slugs. Empty when
	// the card has no Meta.
	DrawsOn string `json:"drawsOn,omitempty"`
	// Rows are the offer's details, in order.
	Rows []CardRow `json:"rows,omitempty"`
	// Sources are the assets the work draws on.
	Sources []CardSource `json:"sources,omitempty"`
	// Footnote closes the card, e.g. what happens after the yes.
	Footnote string `json:"footnote,omitempty"`
	// FootnoteKey is which footnote, one of the CardFootnote slugs, and
	// FootnoteCount the number it is worded with.
	FootnoteKey   string `json:"footnoteKey,omitempty"`
	FootnoteCount int    `json:"footnoteCount,omitzero"`
	// Widen offers the same work over more sources than the narrow default
	// it was offered on, e.g. the campaign's assets instead of the open one.
	Widen *CardWiden `json:"widen,omitempty"`
	// Trail is how each example the work learns from came to be, shown
	// before the yes; the tool's result carries the same trail after it.
	Trail []TrailStep `json:"trail,omitempty"`
}

// The card actions: what the confirm button does.
const (
	// CardActionDraftPosts drafts Count finished posts.
	CardActionDraftPosts = "draft_posts"
	// CardActionPlanIdeas plans Count ideas.
	CardActionPlanIdeas = "plan_ideas"
	// CardActionGeneratePlan regenerates the campaign's content plan.
	CardActionGeneratePlan = "generate_plan"
	// CardActionCreateCampaign creates a campaign.
	CardActionCreateCampaign = "create_campaign"
	// CardActionTag adds tags to Count assets, and may remove some too.
	CardActionTag = "tag"
	// CardActionUntag only removes tags from Count assets.
	CardActionUntag = "untag"
	// CardActionReadBrand reads the brand off a website and assets.
	CardActionReadBrand = "read_brand"
	// CardActionAnswerQuestions answers Count of a brand reading's questions.
	CardActionAnswerQuestions = "answer_questions"
	// CardActionAdd, CardActionUpdate and CardActionRemove add, update or
	// remove Count of the card's Item.
	CardActionAdd    = "add"
	CardActionUpdate = "update"
	CardActionRemove = "remove"
	// CardActionEditBrand makes Count brand changes of more than one kind;
	// its change rows say which.
	CardActionEditBrand = "edit_brand"
)

// The things a card or a row counts.
const (
	CardItemFact     = "fact"
	CardItemVoice    = "voice"
	CardItemAudience = "audience"
)

// What a card's Meta names.
const (
	// CardDrawsOnSources is the card's Sources: "from <title>" for one,
	// "from N assets" for more.
	CardDrawsOnSources = "sources"
	// CardDrawsOnResearch is the research in this chat.
	CardDrawsOnResearch = "research"
)

// The card footnotes.
const (
	// CardFootnoteCampaignSources: the FootnoteCount assets become the new
	// campaign's sources; no posts yet, a content plan can follow.
	CardFootnoteCampaignSources = "campaign_sources"
	// CardFootnoteDraftsLand: the FootnoteCount drafts land in the campaign,
	// ready to edit.
	CardFootnoteDraftsLand = "drafts_land"
	// CardFootnoteBrandReading: the reading runs in the background and takes
	// about two minutes.
	CardFootnoteBrandReading = "brand_reading"
)

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
	// CardRowRecipient is someone the work is sent to, with how they're
	// reached in Note.
	CardRowRecipient = "recipient"
	// CardRowPost is a post the work changes: its title, and its status,
	// platform and date in Note.
	CardRowPost = "post"
	// CardRowMedia is what happens to a post's media.
	CardRowMedia = "media"
	// CardRowPlatform is a platform and the post types it will run.
	CardRowPlatform = "platform"
	// CardRowFixes is what else the work puts right.
	CardRowFixes = "fixes"
	// CardRowPosts is posts the work leaves as they are, with what happens
	// to them in Note, e.g. "they stay, flagged".
	CardRowPosts = "posts"
	// CardRowWebsite is a website the work reads: its homepage and up to
	// nine pages.
	CardRowWebsite = "website"
	// CardRowAnswer is an answer, with its question in Label and where it
	// came from in Note; no Note means the user said it.
	CardRowAnswer = "answer"
	// CardRowChange is Count of Item the work will add, update or remove, by
	// Action, naming the first few in Names.
	CardRowChange = "change"
	// CardRowKept is something the work leaves alone on purpose, with why in
	// Note.
	CardRowKept = "kept"
)

// CardRow is one detail of an offer. Kind lets the client label and lay it
// out; Value is the detail as English text, and the fields after Note are
// the facts it is worded from.
type CardRow struct {
	Kind string `json:"kind"`
	// Label is what Value answers, e.g. an answer row's question, as asked.
	Label string `json:"label,omitempty"`
	Value string `json:"value"`
	// Note explains the value, e.g. why those dates. The model writes it,
	// like the reply.
	Note string `json:"note,omitempty"`
	// Start and End are a dates row's days, YYYY-MM-DD, for the client to
	// format.
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
	// Spread marks a dates row with no day picked: the work is spread from
	// Start to End.
	Spread bool `json:"spread,omitzero"`
	// Platforms are a destination row's platforms, by their names, and
	// Campaign the campaign's name when the work lands in one.
	Platforms []string `json:"platforms,omitempty"`
	Campaign  string   `json:"campaign,omitempty"`
	// Add and Remove are a tags row's tag names.
	Add    []string `json:"add,omitempty"`
	Remove []string `json:"remove,omitempty"`
	// NotGrounded marks a grounding row for work no asset grounds.
	NotGrounded bool `json:"notGrounded,omitzero"`
	// Item, Action and Count are a change row's: Count of Item, one of the
	// CardItem slugs, to add, update or remove. Names are the first few by
	// their names; Count less them is how many more.
	Item   string   `json:"item,omitempty"`
	Action string   `json:"action,omitempty"`
	Count  int      `json:"count,omitzero"`
	Names  []string `json:"names,omitempty"`
}

// CardSource is one asset an offer draws on. Type is the asset's kind, for
// its badge: PDF, DOC, IMG, AUDIO, URL or MD.
type CardSource struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Type  string `json:"type,omitempty"`
}

// CardWiden is a wider source the card offers. Label is the button, e.g.
// "Use the campaign's 6 assets", and Count the assets it names; Instruction
// is the turn the client sends when it is pressed.
type CardWiden struct {
	Note        string `json:"note"`
	Label       string `json:"label"`
	Count       int    `json:"count"`
	Instruction string `json:"instruction"`
}
