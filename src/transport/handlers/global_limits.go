package handlers

import "github.com/ogen-app/ogen/src/domain/platforms"

// The cross-platform upload ceilings are operator-controlled global config
// (CON-292): they read the cached platform_global_limits row instead of
// build-time constants. These thin accessors keep the call sites in
// post_attachments.go / post_video_attachments.go unchanged apart from the call
// parentheses, and degrade to the built-in defaults when config is unavailable.

func maxImageUploadBytes() int64 { return platforms.GlobalLimits().MaxImageUploadBytes }
func maxPDFUploadBytes() int64   { return platforms.GlobalLimits().MaxPDFUploadBytes }
func maxVideoUploadBytes() int64 { return platforms.GlobalLimits().MaxVideoUploadBytes }
func maxAltTextLen() int         { return platforms.GlobalLimits().MaxAltTextChars }
