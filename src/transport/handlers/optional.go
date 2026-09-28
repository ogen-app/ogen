package handlers

import (
	"encoding/json"

	"github.com/ogen-app/ogen/src/domain/models"
)

// Optional distinguishes the three JSON states a request field can be in:
// absent, explicit null, and a concrete value. A plain pointer field collapses
// the first two into nil, so a full-replace PUT cannot tell "leave this alone"
// (key omitted) apart from "clear this" (key set to null).
//
// That distinction only matters for fields the server writes on its own, with
// no client in the loop: the brand refs are stamped by content_plan /
// draft_post, so an ordinary save that omits them would otherwise null a value
// the client may never have seen. Optional keeps absent and null apart so those
// fields survive omission. Use it sparingly — client-authored fields are
// correctly full-replace and should stay plain.
//
// The zero value is the absent state (Present=false), which is what stdlib
// encoding/json leaves a field at when its key is missing; a present key —
// including one whose value is null — invokes UnmarshalJSON and sets Present.
type Optional[T any] struct {
	Present bool // the key appeared in the request body
	Value   *T   // nil when the value was explicit null
}

// UnmarshalJSON records that the key was present and captures its value (nil for
// an explicit null). encoding/json only calls this for keys that appear in the
// body, so an omitted key leaves the zero value (Present=false).
func (o *Optional[T]) UnmarshalJSON(data []byte) error {
	o.Present = true
	if string(data) == "null" {
		o.Value = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

// applyTo overwrites *dst only when the field was present in the body: an
// omitted key leaves the existing value untouched, while a present value or an
// explicit null replaces it (the null clearing the ref).
func (o Optional[T]) applyTo(dst **T) {
	if o.Present {
		*dst = o.Value
	}
}

// applyToValue is applyTo for a non-pointer destination: it overwrites *dst only
// when the field carried a concrete value (present and non-null). Omission — and
// an explicit null, which cannot be represented in a plain value — leave *dst
// untouched. Used for scalar fields that gained a dedicated write path and so
// must survive an unrelated whole-record save (CON-233 use_assets).
func (o Optional[T]) applyToValue(dst *T) {
	if o.Present && o.Value != nil {
		*dst = *o.Value
	}
}

// orZero returns the concrete value the field carried, or the type's zero value
// when it was absent or explicit null. For a slice T the zero value is a nil
// slice (which nullSlice normalises to empty); for a bool it is false. Used on
// the Create path, where there is no stored value to preserve.
func (o Optional[T]) orZero() T {
	if o.Present && o.Value != nil {
		return *o.Value
	}
	var zero T
	return zero
}

// present builds an Optional carrying a concrete value — the present, non-null
// state. For code paths (and tests) that already hold the value.
func present[T any](v T) Optional[T] {
	return Optional[T]{Present: true, Value: &v}
}

// applyOptionalSlice applies a presence-aware id-list to *dst: an omitted key
// leaves the stored slice untouched, so a whole-record save never restates —
// and clobbers — a set that has its own membership endpoints; a
// present array replaces it and an explicit null clears it. Stored values are
// normalised to a non-null empty slice (nullSlice).
func applyOptionalSlice(o Optional[models.StringSlice], dst *models.StringSlice) {
	if o.Present {
		*dst = nullSlice(o.orZero())
	}
}
