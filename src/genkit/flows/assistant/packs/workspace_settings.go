package packs

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
)

// PlatformLookup names a platform the workspace can connect and publish to,
// by its slug; ok false for one it can't.
type PlatformLookup func(slug string) (label string, ok bool)

func autoPublishSpecs(g *genkit.Genkit, d Deps) []assistantkit.Spec {
	if d.AutoPublish == nil || d.ConnectPlatform == nil {
		return nil
	}
	tool := genkit.DefineTool(g, "setAutoPublish",
		"Turns auto-publishing on or off for one platform: whether its scheduled posts go out on their own or wait to be published by hand. Always offered for the user to confirm first.",
		assistantkit.Gated("setAutoPublish", assistantkit.Gate[AutoPublishInput]{
			Precheck: func(ctx context.Context, in AutoPublishInput) error {
				_, err := autoPublishChange(ctx, d, in)
				return err
			},
			Summary: func(in AutoPublishInput) string {
				label, _ := d.ConnectPlatform(slug(in.Platform))
				if in.On {
					return "Auto-publish scheduled " + label + " posts"
				}
				return "Stop auto-publishing " + label + " posts; they'll wait to be published by hand"
			},
		}, assistantkit.Wrap(recorded(workspacePackName, "setAutoPublish", nil, func(ctx context.Context, in AutoPublishInput) (*AutoPublishOutput, error) {
			return setAutoPublish(ctx, d, in)
		}))), ai.WithStrictSchema(false))
	return []assistantkit.Spec{spec(workspacePackName, tool, assistantkit.TierProposal,
		`Call this when the user asks to turn auto-publishing on or off for a platform — "let LinkedIn posts go out on their own", "stop auto-posting to X". platform is its slug, e.g. linkedin. It changes posts scheduled from now on; posts already scheduled keep how they were scheduled. Always offered first, and it can be undone.`)}
}

// AutoPublishInput turns one platform's auto-publishing on or off.
type AutoPublishInput struct {
	Platform string `json:"platform" jsonschema:"description=Platform slug, e.g. linkedin"`
	On       bool   `json:"on"       jsonschema:"description=true to auto-publish, false to publish by hand"`
}

// AutoPublishOutput is the allowlist after the change.
type AutoPublishOutput struct {
	Done        string   `json:"done"`
	AutoPublish []string `json:"autoPublish"`
	UndoID      string   `json:"undoId,omitempty"`
}

type allowlistChange struct{ before, after []string }

func slug(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// autoPublishChange is the allowlist before and after in, refusing an
// unknown platform or a change that changes nothing.
func autoPublishChange(ctx context.Context, d Deps, in AutoPublishInput) (allowlistChange, error) {
	p := slug(in.Platform)
	label, ok := d.ConnectPlatform(p)
	if !ok {
		return allowlistChange{}, assistantkit.Refused(fmt.Sprintf("%q isn't a platform Ogen publishes to; use a slug like linkedin or instagram", in.Platform))
	}
	before, err := d.AutoPublish.List(ctx)
	if err != nil {
		return allowlistChange{}, fmt.Errorf("read auto-publish platforms: %w", err)
	}
	if slices.Contains(before, p) == in.On {
		state := "already auto-publishes"
		if !in.On {
			state = "already waits to be published by hand"
		}
		return allowlistChange{}, assistantkit.Refused(label + " " + state)
	}
	after := slices.DeleteFunc(slices.Clone(before), func(s string) bool { return s == p })
	if in.On {
		after = append(after, p)
		slices.Sort(after)
	}
	return allowlistChange{before: before, after: after}, nil
}

func setAutoPublish(ctx context.Context, d Deps, in AutoPublishInput) (*AutoPublishOutput, error) {
	c, err := autoPublishChange(ctx, d, in)
	if err != nil {
		return nil, err
	}
	// Conditional on what was read, so a change made meanwhile isn't
	// overwritten.
	ok, err := d.AutoPublish.ReplaceIf(ctx, nil, c.before, c.after)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, assistantkit.Refused("auto-publishing was changed meanwhile; check workspaceSetup and ask again")
	}
	assistantkit.Touch(ctx, "auto_publish", "")
	label, _ := d.ConnectPlatform(slug(in.Platform))
	done := "scheduled " + label + " posts now go out on their own"
	if !in.On {
		done = "scheduled " + label + " posts now wait to be published by hand"
	}
	return &AutoPublishOutput{Done: done, AutoPublish: append([]string{}, c.after...),
		UndoID: recordUndo(ctx, d, models.AssistantActionAutoPublish, "auto_publish", assistantundo.AutoPublish(c.before), assistantundo.AutoPublish(c.after))}, nil
}

func connectSpecs(g *genkit.Genkit, d Deps) []assistantkit.Spec {
	if d.ConnectPlatform == nil {
		return nil
	}
	tool := genkit.DefineTool(g, "openConnect",
		"Opens the settings screen's flow to reconnect one connected account, or to connect a new account on a platform. The user finishes it there; nothing is connected or disconnected here.",
		assistantkit.Wrap(recorded(workspacePackName, "openConnect", nil, func(ctx context.Context, in ConnectInput) (*assistant.UIResult, error) {
			return openConnect(ctx, d, in)
		})), ai.WithStrictSchema(false))
	return []assistantkit.Spec{spec(workspacePackName, tool, assistantkit.TierRead,
		`Call this when an account needs reconnecting and the user wants to fix it, or the user asks to connect an account — "reconnect my LinkedIn", "connect Instagram". Pass accountId (from workspaceSetup) to reconnect that account, or platform to connect a new one. It only opens the flow; say the user finishes it on screen.`)}
}

// ConnectInput is the connect flow to open.
type ConnectInput struct {
	AccountID string `json:"accountId,omitempty" jsonschema:"description=A connected account to reconnect, from workspaceSetup"`
	Platform  string `json:"platform,omitempty"  jsonschema:"description=Platform slug to connect a new account on, e.g. instagram"`
}

func openConnect(ctx context.Context, d Deps, in ConnectInput) (*assistant.UIResult, error) {
	c := assistant.UIConnect{Platform: slug(in.Platform)}
	if in.AccountID != "" {
		acct, err := connectedAccount(ctx, d, in.AccountID)
		if err != nil {
			return nil, err
		}
		c = assistant.UIConnect{Platform: acct.Platform, AccountID: acct.ID, Reconnect: true}
	}
	if c.Platform == "" {
		return nil, assistantkit.Refused("which account or platform? pass accountId to reconnect one, or platform to connect one")
	}
	label, ok := d.ConnectPlatform(c.Platform)
	if !ok {
		return nil, assistantkit.Refused(fmt.Sprintf("%q isn't a platform Ogen can connect; use a slug like linkedin or instagram", c.Platform))
	}
	if err := assistant.OpenConnect(ctx, c); err != nil {
		return nil, err
	}
	done := "opening settings to connect an account on " + label
	if c.Reconnect {
		done = "opening settings to reconnect the " + label + " account"
	}
	return &assistant.UIResult{Done: done, Note: "The user's screen changes when your reply arrives; they finish connecting there."}, nil
}

// connectedAccount is one of the workspace's connected accounts, refusing
// an id that isn't.
func connectedAccount(ctx context.Context, d Deps, id string) (*AccountOut, error) {
	setup, err := workspaceSetup(ctx, d)
	if err != nil {
		return nil, err
	}
	for _, a := range setup.Accounts {
		if a.ID == id {
			return &a, nil
		}
	}
	return nil, assistantkit.Refused("that account isn't connected to this workspace; check workspaceSetup")
}
