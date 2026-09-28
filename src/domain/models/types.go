package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// StringSlice is a []string that serialises as a JSON array in a jsonb column.
type StringSlice []string

func (s StringSlice) Value() (driver.Value, error) {
	b, err := json.Marshal(s)
	return string(b), err
}

func (s *StringSlice) Scan(src any) error {
	switch v := src.(type) {
	case string:
		return json.Unmarshal([]byte(v), s)
	case []byte:
		return json.Unmarshal(v, s)
	case nil:
		*s = StringSlice{}
		return nil
	default:
		return fmt.Errorf("StringSlice: cannot scan %T", src)
	}
}

// CampaignPlatform is a target platform entry on a campaign, carrying the
// platform ID and the subset of post types selected for that campaign.
type CampaignPlatform struct {
	ID        string   `json:"id"`
	PostTypes []string `json:"post_types"`
}

// CampaignPlatforms is a []CampaignPlatform that serialises as a JSON array
// in a jsonb column.
type CampaignPlatforms []CampaignPlatform

func (s CampaignPlatforms) Value() (driver.Value, error) {
	b, err := json.Marshal(s)
	return string(b), err
}

func (s *CampaignPlatforms) Scan(src any) error {
	switch v := src.(type) {
	case string:
		return json.Unmarshal([]byte(v), s)
	case []byte:
		return json.Unmarshal(v, s)
	case nil:
		*s = CampaignPlatforms{}
		return nil
	default:
		return fmt.Errorf("CampaignPlatforms: cannot scan %T", src)
	}
}

// ThreadSegment is one message in a threaded post. A thread post
// (PlatformPostType == PostTypeThread) is an ordered list of these: index 0 is
// the root message, 1..N-1 the ordered replies. Only text lives here —
// per-segment media is expressed by PostAttachment.SegmentIndex — so the array
// stays a thin text spine that mirrors into posts.content (the root) for the
// many thread-unaware readers of that field.
type ThreadSegment struct {
	Content string `json:"content"`
}

// ThreadSegments is an ordered []ThreadSegment persisted as a JSON array in a
// jsonb column. Empty for every non-thread post.
type ThreadSegments []ThreadSegment

func (s ThreadSegments) Value() (driver.Value, error) {
	// Normalise nil to an empty array so a Post built without segments still
	// satisfies the NOT NULL DEFAULT '[]' column (a nil slice would marshal to
	// the JSON literal "null"). Every non-thread post relies on this.
	if s == nil {
		return "[]", nil
	}
	b, err := json.Marshal(s)
	return string(b), err
}

func (s *ThreadSegments) Scan(src any) error {
	switch v := src.(type) {
	case string:
		return json.Unmarshal([]byte(v), s)
	case []byte:
		return json.Unmarshal(v, s)
	case nil:
		*s = ThreadSegments{}
		return nil
	default:
		return fmt.Errorf("ThreadSegments: cannot scan %T", src)
	}
}

// RootContent returns segment 0's content — the message mirrored into
// posts.content — or "" when the thread carries no segments.
func (s ThreadSegments) RootContent() string {
	if len(s) == 0 {
		return ""
	}
	return s[0].Content
}

// JSONMap is a free-form map[string]any that serialises as a JSON object in a
// jsonb column. Used for the tenant_activity_events payload, where each
// activity type carries its own small bag of fields.
type JSONMap map[string]any

func (m JSONMap) Value() (driver.Value, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	return string(b), err
}

func (m *JSONMap) Scan(src any) error {
	switch v := src.(type) {
	case string:
		if v == "" {
			*m = nil
			return nil
		}
		return json.Unmarshal([]byte(v), m)
	case []byte:
		if len(v) == 0 {
			*m = nil
			return nil
		}
		return json.Unmarshal(v, m)
	case nil:
		*m = nil
		return nil
	default:
		return fmt.Errorf("JSONMap: cannot scan %T", src)
	}
}

// PostTypeMap is a map[string]string that serialises as a JSON object in a
// jsonb column. Keys are post-type slugs, values are display names.
type PostTypeMap map[string]string

func (m PostTypeMap) Value() (driver.Value, error) {
	b, err := json.Marshal(m)
	return string(b), err
}

func (m *PostTypeMap) Scan(src any) error {
	switch v := src.(type) {
	case string:
		return json.Unmarshal([]byte(v), m)
	case []byte:
		return json.Unmarshal(v, m)
	case nil:
		*m = PostTypeMap{}
		return nil
	default:
		return fmt.Errorf("PostTypeMap: cannot scan %T", src)
	}
}

// StringMap is a generic map[string]string that serialises as a JSON object in
// a jsonb column. Used for the email_templates variables docs: key =
// the [[ .Var ]] placeholder name, value = a human explanation.
type StringMap map[string]string

func (m StringMap) Value() (driver.Value, error) {
	if m == nil {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	return string(b), err
}

func (m *StringMap) Scan(src any) error {
	switch v := src.(type) {
	case string:
		if v == "" {
			*m = StringMap{}
			return nil
		}
		return json.Unmarshal([]byte(v), m)
	case []byte:
		if len(v) == 0 {
			*m = StringMap{}
			return nil
		}
		return json.Unmarshal(v, m)
	case nil:
		*m = StringMap{}
		return nil
	default:
		return fmt.Errorf("StringMap: cannot scan %T", src)
	}
}
