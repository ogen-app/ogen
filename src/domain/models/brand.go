package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// Brand materials (CON-228) — the workspace-level material that makes generated
// content its own: voices, audiences, guardrails, look and templates. The wire
// shapes here mirror the ui repo's `components/brand/types.ts` exactly (camelCase
// json tags included), because the whole prototype was built so that when this
// endpoint lands each stubbed service body becomes one apiJson call and nothing
// above it changes. See the ui repo's docs/brand-materials.md for the argument.
//
// Every model embeds TenantScoped, so tenant isolation is enforced centrally by
// bun hooks (CON-97 §6) — reads/updates/deletes are scoped and inserts stamped,
// and no repository method can forget.

// ── jsonb value types ───────────────────────────────────────────────────────

// VoiceRules is how a voice handles the things that most obviously give it away.
type VoiceRules struct {
	Emoji     string `json:"emoji"`
	Hashtags  string `json:"hashtags"`
	Formality string `json:"formality"`
	Person    string `json:"person"`
	Length    string `json:"length"`
	Opening   string `json:"opening"`
	Closing   string `json:"closing"`
}

func (r VoiceRules) Value() (driver.Value, error) {
	b, err := json.Marshal(r)
	return string(b), err
}

func (r *VoiceRules) Scan(src any) error {
	switch v := src.(type) {
	case string:
		if v == "" {
			*r = VoiceRules{}
			return nil
		}
		return json.Unmarshal([]byte(v), r)
	case []byte:
		if len(v) == 0 {
			*r = VoiceRules{}
			return nil
		}
		return json.Unmarshal(v, r)
	case nil:
		*r = VoiceRules{}
		return nil
	default:
		return fmt.Errorf("VoiceRules: cannot scan %T", v)
	}
}

// BrandOrigin is where a piece of material came from — a template is forked,
// never linked, so without this line nothing connects an entry to what it
// started as. Modelled as one struct over the discriminated union; omitempty
// keeps the wire shape to just `kind` plus whichever field the kind carries.
type BrandOrigin struct {
	Kind         string `json:"kind"`
	TemplateName string `json:"templateName,omitempty"`
	URL          string `json:"url,omitempty"`
	Count        int    `json:"count,omitzero"`
	FromPost     string `json:"fromPost,omitempty"`
}

func (o BrandOrigin) Value() (driver.Value, error) {
	b, err := json.Marshal(o)
	return string(b), err
}

func (o *BrandOrigin) Scan(src any) error {
	switch v := src.(type) {
	case string:
		if v == "" {
			*o = BrandOrigin{Kind: "blank"}
			return nil
		}
		return json.Unmarshal([]byte(v), o)
	case []byte:
		if len(v) == 0 {
			*o = BrandOrigin{Kind: "blank"}
			return nil
		}
		return json.Unmarshal(v, o)
	case nil:
		*o = BrandOrigin{Kind: "blank"}
		return nil
	default:
		return fmt.Errorf("BrandOrigin: cannot scan %T", v)
	}
}

// BrandLogo is a logo variant with a declared job — not a folder of files.
type BrandLogo struct {
	ID  string `json:"id"`
	Job string `json:"job"`
	URL string `json:"url"`
}

// BrandLogos serialises as a jsonb array.
type BrandLogos []BrandLogo

func (s BrandLogos) Value() (driver.Value, error) { b, err := json.Marshal(s); return string(b), err }

func (s *BrandLogos) Scan(src any) error { return scanJSONSlice(src, s) }

// BrandColor is a colour with a role. A swatch dump is a palette nobody can apply.
type BrandColor struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	Hex  string `json:"hex"`
}

// BrandPalette serialises as a jsonb array.
type BrandPalette []BrandColor

func (s BrandPalette) Value() (driver.Value, error) { b, err := json.Marshal(s); return string(b), err }

func (s *BrandPalette) Scan(src any) error { return scanJSONSlice(src, s) }

// TemplateRatio is one full-canvas asset for one aspect ratio.
type TemplateRatio struct {
	Ratio string `json:"ratio"`
	URL   string `json:"url"`
}

// TemplateRatios serialises as a jsonb array.
type TemplateRatios []TemplateRatio

func (s TemplateRatios) Value() (driver.Value, error) {
	b, err := json.Marshal(s)
	return string(b), err
}

func (s *TemplateRatios) Scan(src any) error { return scanJSONSlice(src, s) }

// scanJSONSlice unmarshals a jsonb column (string/[]byte/nil) into dst, a
// pointer to a slice. nil / empty leaves dst as an empty (non-nil) slice so the
// wire shape is `[]`, never `null`.
func scanJSONSlice(src, dst any) error {
	switch v := src.(type) {
	case string:
		if v == "" {
			return nil
		}
		return json.Unmarshal([]byte(v), dst)
	case []byte:
		if len(v) == 0 {
			return nil
		}
		return json.Unmarshal(v, dst)
	case nil:
		return nil
	default:
		return fmt.Errorf("jsonb slice: cannot scan %T", v)
	}
}

// BrandUsage is how much of the workspace's output a piece of material is behind.
// Two numbers, because they answer different questions: drafts are still ours to
// regenerate, published posts are out in the world. Derived, never stored (FR7);
// zero until CON-245 wires the post→voice reference.
type BrandUsage struct {
	Drafts    int `json:"drafts"`
	Published int `json:"published"`
}

// ── Entities ────────────────────────────────────────────────────────────────

// BrandVoice — a name, a when-to-use, three-to-eight real samples, explicit
// rules and optional per-channel notes. The samples are the voice.
type BrandVoice struct {
	bun.BaseModel `bun:"table:brand_voices,alias:bv" swaggerignore:"true"`
	TenantScoped

	ID           string      `bun:"id,pk"                                        json:"id"`
	Name         string      `bun:"name,notnull"                                 json:"name"`
	WhenToUse    string      `bun:"when_to_use,notnull"                          json:"whenToUse"`
	IsDefault    bool        `bun:"is_default,notnull"                           json:"isDefault"`
	Samples      StringSlice `bun:"samples,notnull,type:jsonb"                   json:"samples"`
	Rules        VoiceRules  `bun:"rules,notnull,type:jsonb"                     json:"rules"`
	ChannelNotes StringMap   `bun:"channel_notes,notnull,type:jsonb"             json:"channelNotes"`
	Origin       BrandOrigin `bun:"origin,notnull,type:jsonb"                    json:"origin"`
	// Summary is our reading of the samples, generated not authored (FR5);
	// write-ignored. "" is the honest rendering of a voice with nothing behind it.
	Summary   string    `bun:"summary,notnull"                              json:"summary"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"-"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updatedAt"`

	// Derived, never stored (FR7). Zero until CON-245.
	Usage       BrandUsage `bun:"-" json:"usage"`
	PostsBehind int        `bun:"-" json:"postsBehind,omitempty"`
}

// BrandAudience — who the posts are written to, described by what follows from
// it (where they read, what makes them scroll past, what they need to believe a
// number). The three consequence lines are the design, not decoration.
type BrandAudience struct {
	bun.BaseModel `bun:"table:brand_audiences,alias:ba" swaggerignore:"true"`
	TenantScoped

	ID              string      `bun:"id,pk"                                        json:"id"`
	Name            string      `bun:"name,notnull"                                 json:"name"`
	Who             string      `bun:"who,notnull"                                  json:"who"`
	ReadsOn         string      `bun:"reads_on,notnull"                             json:"readsOn"`
	ScrollsPastWhen string      `bun:"scrolls_past_when,notnull"                    json:"scrollsPastWhen"`
	BelievesWhen    string      `bun:"believes_when,notnull"                        json:"believesWhen"`
	Origin          BrandOrigin `bun:"origin,notnull,type:jsonb"                    json:"origin"`
	Summary         string      `bun:"summary,notnull"                              json:"summary"`
	CreatedAt       time.Time   `bun:"created_at,notnull,default:current_timestamp" json:"-"`
	UpdatedAt       time.Time   `bun:"updated_at,notnull,default:current_timestamp" json:"updatedAt"`

	Usage BrandUsage `bun:"-" json:"usage"`
}

// BrandGuardrails — facts, claims we may make, claims we may never make, banned
// words, and the disclaimer every post carries. The singleton with real weight.
// One row per tenant; id is internal (the client sees no id for a singleton).
type BrandGuardrails struct {
	bun.BaseModel `bun:"table:brand_guardrails,alias:bg" swaggerignore:"true"`
	TenantScoped

	ID          string      `bun:"id,pk"                                        json:"-"`
	Facts       StringSlice `bun:"facts,notnull,type:jsonb"                     json:"facts"`
	MayClaim    StringSlice `bun:"may_claim,notnull,type:jsonb"                 json:"mayClaim"`
	NeverClaim  StringSlice `bun:"never_claim,notnull,type:jsonb"               json:"neverClaim"`
	BannedWords StringSlice `bun:"banned_words,notnull,type:jsonb"              json:"bannedWords"`
	Disclaimer  string      `bun:"disclaimer,notnull"                           json:"disclaimer"`
	CreatedAt   time.Time   `bun:"created_at,notnull,default:current_timestamp" json:"-"`
	UpdatedAt   time.Time   `bun:"updated_at,notnull,default:current_timestamp" json:"updatedAt"`
}

// BrandLook — logos with a declared job, colours with roles, type, and imagery.
// The other singleton; one row per tenant.
type BrandLook struct {
	bun.BaseModel `bun:"table:brand_look,alias:bl" swaggerignore:"true"`
	TenantScoped

	ID              string       `bun:"id,pk"                                        json:"-"`
	Logos           BrandLogos   `bun:"logos,notnull,type:jsonb"                     json:"logos"`
	Palette         BrandPalette `bun:"palette,notnull,type:jsonb"                   json:"palette"`
	Typefaces       StringSlice  `bun:"typefaces,notnull,type:jsonb"                 json:"typefaces"`
	ReferenceImages StringSlice  `bun:"reference_images,notnull,type:jsonb"          json:"referenceImages"`
	CreatedAt       time.Time    `bun:"created_at,notnull,default:current_timestamp" json:"-"`
	UpdatedAt       time.Time    `bun:"updated_at,notnull,default:current_timestamp" json:"updatedAt"`
}

// BrandTemplate — a full-canvas frame per platform and per ratio. Called a
// template (the user's word), built as an overlay (ours). No layout reflow.
type BrandTemplate struct {
	bun.BaseModel `bun:"table:brand_templates,alias:bt" swaggerignore:"true"`
	TenantScoped

	ID        string         `bun:"id,pk"                                        json:"id"`
	Name      string         `bun:"name,notnull"                                 json:"name"`
	Role      string         `bun:"role,notnull"                                 json:"role"`
	Ratios    TemplateRatios `bun:"ratios,notnull,type:jsonb"                    json:"ratios"`
	IsDefault bool           `bun:"is_default,notnull"                           json:"isDefault"`
	Platforms StringSlice    `bun:"platforms,notnull,type:jsonb"                 json:"platforms"`
	Origin    BrandOrigin    `bun:"origin,notnull,type:jsonb"                    json:"origin"`
	CreatedAt time.Time      `bun:"created_at,notnull,default:current_timestamp" json:"-"`
	UpdatedAt time.Time      `bun:"updated_at,notnull,default:current_timestamp" json:"updatedAt"`
}

// BrandData is everything the Brand screen renders — the one aggregate GET
// /api/brand answers with. Every field is present and every list may be empty:
// an omitted key and an empty slot are different things here (an empty named
// slot is a to-do, an absent one says nothing at all).
type BrandData struct {
	Voices     []BrandVoice     `json:"voices"`
	Audiences  []BrandAudience  `json:"audiences"`
	Guardrails *BrandGuardrails `json:"guardrails"`
	Look       *BrandLook       `json:"look"`
	Templates  []BrandTemplate  `json:"templates"`
}
