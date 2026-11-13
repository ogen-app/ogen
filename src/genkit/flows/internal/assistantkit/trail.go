package assistantkit

// TrailAsset is an asset on a trail. Type is the asset's kind, for its
// badge, as on a CardSource. An idea's asset says where in it the idea
// came from.
type TrailAsset struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Type  string `json:"type,omitempty"`
	Passage
}

// Passage is where in an asset an idea came from, to open the asset there.
// Every field is empty when no passage was recorded: the idea predates it, or
// the asset's body has no chunks.
type Passage struct {
	ChunkID string `json:"chunkId,omitempty"`
	// Label is the passage as cited: "p. 12", "Slide 4", "12:03–12:47".
	Label   string `json:"label,omitempty"`
	Page    *int   `json:"page,omitempty"`
	PageEnd *int   `json:"pageEnd,omitempty"`
	Slide   *int   `json:"slide,omitempty"`
	StartMS *int64 `json:"startMs,omitempty"`
	EndMS   *int64 `json:"endMs,omitempty"`
}

// TrailAnalysis is the analysis an idea was distilled by: the asset's
// workspace lens, or the campaign's, named.
type TrailAnalysis struct {
	ID           string `json:"id"`
	CampaignID   string `json:"campaignId,omitempty"`
	CampaignName string `json:"campaignName,omitempty"`
}

// TrailIdea is an idea an example was made from, the analysis it came by
// and the asset it came from.
type TrailIdea struct {
	ID       string         `json:"id"`
	Title    string         `json:"title"`
	Analysis *TrailAnalysis `json:"analysis,omitempty"`
	Asset    *TrailAsset    `json:"asset,omitempty"`
	// Passages are where in Asset the idea was distilled from, as its
	// analysis cites them: "p. 12", "Slide 4", "12:03–12:47". None for an
	// idea not distilled from that asset.
	Passages []string `json:"passages,omitempty"`
}

// TrailStep is how one example post came to be: its engagement rate, the
// ideas it was made from and the assets it used.
type TrailStep struct {
	PostID string `json:"postId"`
	Title  string `json:"title"`
	// Rate is the post's engagement rate as the publisher reports it, the
	// posts list's engagement_rate, when it has figures.
	Rate   *float64     `json:"rate,omitempty"`
	Ideas  []TrailIdea  `json:"ideas"`
	Assets []TrailAsset `json:"assets"`
	// NoTrail is an example with no ideas or assets recorded: its own text
	// is the only source, and the new ideas cite it.
	NoTrail bool `json:"noTrail,omitempty"`
}
