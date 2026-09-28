package enrich_brief

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
)

// contextCacheTTL controls how long a cached context block stays valid.
// Matches Anthropic's prompt-cache TTL so a repeated enrich (e.g. a retry)
// reuses both our rendered context and the upstream cache window.
const contextCacheTTL = 5 * time.Minute

// briefContext holds the rendered prompts ready for the model call.
type briefContext struct {
	SystemPrompt string // system instructions (stable across calls)
	ContextBlock string // campaign + type + phases + language + instruction
}

// contextCacheEntry is a cached briefContext keyed by campaign ID.
// fingerprint captures every field the template renders — any change to it
// invalidates the entry. Tracked explicitly (not field-by-field diffing)
// so adding a new prompt field is a one-line update to briefFingerprint.
type contextCacheEntry struct {
	ctx         *briefContext
	fingerprint string
	expiresAt   time.Time
}

var (
	contextCache   = map[string]*contextCacheEntry{}
	contextCacheMu sync.Mutex
)

// briefFingerprint returns a stable string that changes whenever any
// prompt-affecting input changes: the campaign title, its type (label +
// description + phases), the output language, or the per-request
// instruction. Missing a field here means a stale brief on the next call.
func briefFingerprint(c *models.Campaign, instruction string) string {
	var b strings.Builder
	b.WriteString(c.Name)
	b.WriteByte('\x1f')
	b.WriteString(c.CampaignTypeID)
	b.WriteByte('\x1f')
	if c.CampaignType != nil {
		b.WriteString(c.CampaignType.Label)
		b.WriteByte('\x1f')
		b.WriteString(c.CampaignType.Description)
		b.WriteByte('\x1f')
		for _, p := range c.CampaignType.Phases {
			fmt.Fprintf(&b, "%d:%s:%s|", p.Sequence, p.Name, p.Purpose)
		}
	}
	b.WriteByte('\x1f')
	b.WriteString(c.Language)
	b.WriteByte('\x1f')
	b.WriteString(instruction)
	return b.String()
}

// assembleContextCached returns the cached context for the campaign if the
// fingerprint matches and the TTL hasn't expired. Otherwise it assembles a
// fresh context, caches it, and returns it.
func assembleContextCached(
	ctx context.Context,
	campaign *models.Campaign,
	instruction string,
	repos EnrichBriefRepos,
	systemTmpl, contextTmpl *template.Template,
) (*briefContext, error) {
	fp := briefFingerprint(campaign, instruction)

	contextCacheMu.Lock()
	if entry, ok := contextCache[campaign.ID]; ok {
		if time.Now().Before(entry.expiresAt) && entry.fingerprint == fp {
			contextCacheMu.Unlock()
			return entry.ctx, nil
		}
		delete(contextCache, campaign.ID)
	}
	contextCacheMu.Unlock()

	bctx, err := assembleContext(ctx, campaign, instruction, repos, systemTmpl, contextTmpl)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	contextCacheMu.Lock()
	// Opportunistic eviction: drop every expired entry before inserting, so
	// the map can't grow unbounded with stale entries for campaigns enriched
	// once and never re-read. Inserts only happen on a cache miss (followed
	// by a model call), so this O(n) sweep is negligible.
	for id, e := range contextCache {
		if now.After(e.expiresAt) {
			delete(contextCache, id)
		}
	}
	contextCache[campaign.ID] = &contextCacheEntry{
		ctx:         bctx,
		fingerprint: fp,
		expiresAt:   now.Add(contextCacheTTL),
	}
	contextCacheMu.Unlock()

	return bctx, nil
}

func assembleContext(
	ctx context.Context,
	campaign *models.Campaign,
	instruction string,
	repos EnrichBriefRepos,
	systemTmpl, contextTmpl *template.Template,
) (*briefContext, error) {
	// CampaignRepository.GetByID hydrates CampaignType (with phases). Fall
	// back to a direct lookup if a caller handed us an un-hydrated campaign.
	ct := campaign.CampaignType
	if ct == nil && campaign.CampaignTypeID != "" {
		loaded, err := repos.CampaignTypes.GetByID(ctx, campaign.CampaignTypeID)
		if err != nil {
			return nil, fmt.Errorf("load campaign type: %w", err)
		}
		ct = loaded
	}

	data := contextTemplateData{
		CampaignName: campaign.Name,
		Language:     campaign.Language,
		Instruction:  instruction,
	}
	if ct != nil {
		data.TypeName = ct.Name
		data.TypeLabel = ct.Label
		data.TypeDescription = ct.Description
		data.Phases = make([]phaseInfo, 0, len(ct.Phases))
		for _, p := range ct.Phases {
			data.Phases = append(data.Phases, phaseInfo{
				Sequence: p.Sequence,
				Name:     p.Name,
				Purpose:  p.Purpose,
			})
		}
	}

	systemPrompt, err := flowkit.RenderTemplate(systemTmpl, data)
	if err != nil {
		return nil, fmt.Errorf("render system prompt: %w", err)
	}
	contextBlock, err := flowkit.RenderTemplate(contextTmpl, data)
	if err != nil {
		return nil, fmt.Errorf("render context block: %w", err)
	}

	return &briefContext{
		SystemPrompt: systemPrompt,
		ContextBlock: contextBlock,
	}, nil
}

type contextTemplateData struct {
	CampaignName    string
	TypeName        string
	TypeLabel       string
	TypeDescription string
	Phases          []phaseInfo
	Language        string
	Instruction     string
}

type phaseInfo struct {
	Sequence int
	Name     string
	Purpose  string
}
