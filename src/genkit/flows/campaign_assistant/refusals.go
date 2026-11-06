package campaign_assistant

import (
	"fmt"
	"maps"
	"slices"

	"github.com/ogen-app/ogen/src/domain/models"
)

// allowedPostTypes returns the post-type slugs the campaign may publish on a
// platform: the campaign's own selection when it made one, otherwise every
// type the platform supports. It mirrors content_plan's resolvePlatforms, so a
// type that passes here also passes the engine.
func allowedPostTypes(c *models.Campaign, platformID string) []string {
	for _, tp := range c.TargetPlatforms {
		if tp.ID == platformID && len(tp.PostTypes) > 0 {
			return tp.PostTypes
		}
	}
	for _, p := range c.Platforms {
		if p.ID == platformID {
			return slices.Sorted(maps.Keys(p.PostTypes))
		}
	}
	return nil
}

// unsupportedPostType reports the first requested platform that cannot take
// postType, with the types it can take. An empty postType means the platform
// default and is always fine.
func unsupportedPostType(c *models.Campaign, platformIDs []string, postType string) (reason string, alternatives []string, bad bool) {
	if postType == "" {
		return "", nil, false
	}
	for _, id := range platformIDs {
		allowed := allowedPostTypes(c, id)
		if len(allowed) == 0 || slices.Contains(allowed, postType) {
			continue
		}
		return fmt.Sprintf("%s in this campaign doesn't take %q posts.", platformName(c, id), postType), allowed, true
	}
	return "", nil, false
}

func platformName(c *models.Campaign, id string) string {
	for _, p := range c.Platforms {
		if p.ID == id {
			return p.Name
		}
	}
	return id
}
