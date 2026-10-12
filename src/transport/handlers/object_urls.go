package handlers

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// objectURL mints a link a browser can load for the stored object at key: a
// presigned GET valid for PresignedURLTTL, or the plain public URL when the
// deployment opted in to public reads (publicRead, STORAGE_PUBLIC_READ). It
// returns nil when there is nothing to link or signing fails — never a public
// fallback, which a private bucket answers with 403. Clients treat a missing URL
// as "nothing to show".
//
// A signed URL expires; clients refetch the resource (on focus, through their
// polls) and so pick up a fresh one before a page left open goes stale.
func objectURL(ctx context.Context, store storage.Storage, publicRead bool, key string) *string {
	if store == nil || key == "" {
		return nil
	}
	if publicRead {
		return new(store.PublicURL(key))
	}
	u, err := store.PresignedGetURL(ctx, key, PresignedURLTTL)
	if err != nil {
		slog.WarnContext(ctx, "presign object url failed", logging.AttrComponent, "handlers.object_urls",
			"key", key, logging.AttrError, err)
		return nil
	}
	return &u
}

// linkChars is what a link in Markdown or HTML can't contain unescaped, so a
// match never runs past the link it starts in.
const linkChars = `[^\s()<>\[\]"']`

// relinkKeys rewrites every link in content that points at one of keys —
// whatever its base URL, with or without a query (a presigned signature) — to
// the link mint returns for that key. A key mint maps to "" is left alone.
// Scraped Markdown embeds its mirrored images by URL, so this is how a read
// swaps the stored links for loadable ones and a write swaps them back.
func relinkKeys(content string, keys []string, mint func(key string) string) string {
	if content == "" || len(keys) == 0 {
		return content
	}
	alts := make([]string, 0, len(keys))
	for _, k := range keys {
		if k != "" && strings.Contains(content, k) {
			alts = append(alts, regexp.QuoteMeta(k))
		}
	}
	if len(alts) == 0 {
		return content
	}
	// The greedy tail takes the rest of the link, so the suffix check below
	// sees its whole path: ".../1.png.backup" is not the key ".../1.png".
	re := regexp.MustCompile(linkChars + `*?/(?:` + strings.Join(alts, "|") + `)` + linkChars + `*`)
	return re.ReplaceAllStringFunc(content, func(m string) string {
		path, _, _ := strings.Cut(m, "?")
		for _, k := range keys {
			if k != "" && strings.HasSuffix(path, "/"+k) {
				if u := mint(k); u != "" {
					return u
				}
				return m
			}
		}
		return m
	})
}
