// Package embedopts centralises the per-request options passed to the Gemini
// Embedding 2 model (CON-101). It is a leaf package importing only the genai
// SDK so every embed call site — the document-side flows (embed_asset,
// process_pdf) and the query-side consumers (content_plan, post_assistant) —
// can share the same config without import cycles.
package embedopts

import "google.golang.org/genai"

// Dimensions is the embedding output dimensionality requested on every call.
// It is set once at boot from config.EmbedDimensions and must match the
// assets_chunks.embedding halfvec(N) column. Defaults to Gemini's native 3072.
var Dimensions int32 = 3072

// Availabler is implemented by the reloadable Gemini embedder (CON-104), whose
// backing key can be set / rotated / cleared at runtime via the secrets API.
// Available reports whether it can currently serve embed requests.
type Availabler interface{ Available() bool }

// Available reports whether e can currently serve embed requests. It lives here
// — the shared leaf — so every embed call site can gate on availability without
// importing the embedding package (which would cycle). A nil embedder, or a
// reloadable one with no gemini_api_key configured, is unavailable; any other
// (non-reloadable) embedder is assumed available. e is typed any so both
// ai.Embedder and the per-worker embedder interfaces can be passed.
func Available(e any) bool {
	if e == nil {
		return false
	}
	if a, ok := e.(Availabler); ok {
		return a.Available()
	}
	return true
}

// Document returns the embed config for content that is stored and later
// searched against (chunks of assets/PDFs). RETRIEVAL_DOCUMENT tells Gemini to
// optimise the vector for the corpus side of a retrieval task.
//
// Note: we deliberately do not set AutoTruncate — the Gemini Developer API
// rejects that parameter ("autoTruncate parameter is not supported"). The
// chunker keeps every chunk well under Gemini's 8192-token input limit, so
// truncation never applies anyway.
func Document() *genai.EmbedContentConfig {
	return &genai.EmbedContentConfig{
		TaskType:             "RETRIEVAL_DOCUMENT",
		OutputDimensionality: new(Dimensions),
	}
}

// Query returns the embed config for a search query embedded at request time
// (content-plan ranking, the assistant's searchAssetChunks tool).
// RETRIEVAL_QUERY optimises the vector for the query side of the same task.
func Query() *genai.EmbedContentConfig {
	return &genai.EmbedContentConfig{
		TaskType:             "RETRIEVAL_QUERY",
		OutputDimensionality: new(Dimensions),
	}
}
