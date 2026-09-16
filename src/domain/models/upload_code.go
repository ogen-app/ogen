package models

// Stable, machine-readable rejection codes carried BESIDE the human-readable
// message on every upload / image-processing outcome (CON-281). The client
// matches on the code and falls back to the prose message when the code is
// unknown, so a code added here costs nothing until the client reads it — new
// verdicts can ship without a coordinated front-end release.
//
// The code names the KIND of failure, never its parameters: numeric caps
// (max MB, max pixels) stay in the message so the client never holds a second,
// drift-prone copy of a limit the server owns.
//
// Two surfaces consume these:
//   - the synchronous upload result (content-bank batch `uploadResult.Code`
//     and the per-request post-attachment `{code,error}` body), and
//   - the asynchronous content-bank vision run (image_extractions.failure_code,
//     surfaced by the extraction status endpoint + SSE).
//
// Some service-side verdicts (vector vs oversize-pixels vs corrupt) collapse
// into the coarse buckets below in Phase 1; the image.v1 RejectedCode enum
// (Phase 2) lets ogen map them to the finer codes without a client change.
const (
	// UploadCodeExtensionNotAllowed: the filename extension is not one we accept
	// at all (not .md / .pdf / a known image / a known office-or-text document).
	UploadCodeExtensionNotAllowed = "extension_not_allowed"

	// UploadCodeUnsupportedMediaType: the extension routed to a handler but the
	// specific type is not supported — an image type we don't take, a document
	// type outside the office/text matrix, or a legacy/encrypted Office container.
	UploadCodeUnsupportedMediaType = "unsupported_media_type"

	// UploadCodeVectorRejected: SVG / vector artwork — raster images only.
	UploadCodeVectorRejected = "vector_rejected"

	// UploadCodeTooLarge: over the per-kind upload byte ceiling.
	UploadCodeTooLarge = "too_large"

	// UploadCodeEmptyFile: a zero-length upload.
	UploadCodeEmptyFile = "empty_file"

	// UploadCodeInvalidFile: the bytes are not a readable file of the sniffed kind
	// (corrupt/undecodable image or PDF).
	UploadCodeInvalidFile = "invalid_file"

	// UploadCodeDimensionsExceeded: over the pixel-area / dimension ceiling.
	UploadCodeDimensionsExceeded = "dimensions_exceeded"

	// UploadCodeQuotaExceeded: the tenant's usage / cost cap was hit before the
	// (paid) processing spend — terminal, and distinct from a transient failure.
	UploadCodeQuotaExceeded = "quota_exceeded"

	// UploadCodeServiceUnavailable: a required processing service is unwired or
	// temporarily unreachable — the client may retry later.
	UploadCodeServiceUnavailable = "service_unavailable"

	// UploadCodeExtractionPartial: the image was described and is searchable, but
	// structured extraction did not complete — retriable, not a hard failure.
	UploadCodeExtractionPartial = "extraction_partial"

	// UploadCodeInternalError: an unexpected server-side failure (id generation,
	// storage write, DB insert). Generic — the client should offer a retry.
	UploadCodeInternalError = "internal_error"
)

// The image.v1.RejectedCode enum name → upload code mapping lives in the image
// gRPC client (src/transport/grpc/client/image), keyed off the generated enum so
// a proto rename is a compile error — the domain layer stays free of proto types.

// UploadRejectMessage is the default tenant-visible sentence for a reject code,
// used where the reject is surfaced without a more specific message of its own
// (the fine-grained service rejects of CON-281 Phase 2). The numeric caps are
// deliberately NOT restated here — the client reads those from context — so this
// never holds a second copy of a limit the server owns.
func UploadRejectMessage(code string) string {
	switch code {
	case UploadCodeVectorRejected:
		return "SVG / vector images are not supported — upload a raster image (JPEG, PNG, WebP, GIF, HEIC, AVIF, TIFF, or BMP)"
	case UploadCodeUnsupportedMediaType:
		return "unsupported image format"
	case UploadCodeTooLarge:
		return "the image is too large to process"
	case UploadCodeDimensionsExceeded:
		return "the image dimensions are too large to process"
	default:
		return "uploaded file is not a readable image"
	}
}
