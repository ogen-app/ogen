package repository

// Membership of an asset in a campaign (campaigns.asset_ids) or a post
// (posts.used_asset_ids) lives in one jsonb string-array column. These helpers
// build the SET expressions that mutate that set in a single atomic UPDATE, so a
// one-id change never costs a full-record write and two concurrent adds of
// different ids both survive: each is one UPDATE that reads and rewrites the
// column, serialized on the row lock rather than a lost-update Go read-modify-
// write. The column name is a trusted in-code constant, never user input, so
// string interpolation here is safe.

// jsonbIDUnionSet returns a bun Set expression that unions an incoming jsonb
// array (bound to the single `?` placeholder) into the named jsonb string-array
// column. The result is deduplicated and order-preserving: ids already present
// keep their position, genuinely new ids are appended in input order, and any id
// appearing more than once collapses to its first occurrence. Concatenate,
// number with ORDINALITY, keep the earliest ordinal per distinct element, then
// re-aggregate in that order. COALESCE guards the all-empty case (jsonb_agg over
// zero rows is NULL) back to an empty array so the column never goes null.
func jsonbIDUnionSet(column string) string {
	return column + ` = (
		SELECT COALESCE(jsonb_agg(elem ORDER BY ord), '[]'::jsonb)
		FROM (
			SELECT elem, MIN(ord) AS ord
			FROM jsonb_array_elements(` + column + ` || ?::jsonb) WITH ORDINALITY AS t(elem, ord)
			GROUP BY elem
		) d
	)`
}

// jsonbIDRemoveSet returns a bun Set expression that drops every occurrence of a
// single id (bound to `?`) from the named jsonb string-array column. The `-`
// operator is a no-op when the id is absent, so removal is idempotent.
func jsonbIDRemoveSet(column string) string {
	return column + " = " + column + " - ?"
}
