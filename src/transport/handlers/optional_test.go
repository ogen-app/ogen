package handlers

import (
	"encoding/json"
	"testing"
)

// TestOptionalUnmarshal locks the three-state decode: an omitted key
// stays absent, an explicit null is present-and-nil, and a value is present.
func TestOptionalUnmarshal(t *testing.T) {
	type body struct {
		Voice Optional[string] `json:"brand_voice_id"`
	}
	cases := []struct {
		name        string
		json        string
		wantPresent bool
		wantNil     bool
		wantVal     string
	}{
		{"absent", `{}`, false, true, ""},
		{"explicit null", `{"brand_voice_id":null}`, true, true, ""},
		{"value", `{"brand_voice_id":"v-1"}`, true, false, "v-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b body
			if err := json.Unmarshal([]byte(tc.json), &b); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if b.Voice.Present != tc.wantPresent {
				t.Errorf("Present = %v, want %v", b.Voice.Present, tc.wantPresent)
			}
			if (b.Voice.Value == nil) != tc.wantNil {
				t.Errorf("Value nil = %v, want %v", b.Voice.Value == nil, tc.wantNil)
			}
			if tc.wantVal != "" && (b.Voice.Value == nil || *b.Voice.Value != tc.wantVal) {
				t.Errorf("Value = %v, want %q", b.Voice.Value, tc.wantVal)
			}
		})
	}
}

// TestOptionalApplyTo locks the application rule: absent leaves the target
// alone, explicit null clears it, a value overwrites it.
func TestOptionalApplyTo(t *testing.T) {
	old := "old"

	// Absent → untouched.
	dst := &old
	var absent Optional[string]
	absent.applyTo(&dst)
	if dst == nil || *dst != "old" {
		t.Errorf("absent: dst = %v, want unchanged \"old\"", dst)
	}

	// Present null → cleared.
	dst = &old
	nullOpt := Optional[string]{Present: true, Value: nil}
	nullOpt.applyTo(&dst)
	if dst != nil {
		t.Errorf("null: dst = %v, want nil", dst)
	}

	// Present value → set.
	dst = nil
	newVal := "new"
	valOpt := Optional[string]{Present: true, Value: &newVal}
	valOpt.applyTo(&dst)
	if dst == nil || *dst != "new" {
		t.Errorf("value: dst = %v, want \"new\"", dst)
	}
}
