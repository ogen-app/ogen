package zernio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// BootstrapState carries a remote bootstrap between steps. river-service
// holds it opaquely and hands it back with each Zernio result.
type BootstrapState struct {
	Phase    string          `json:"phase,omitempty"`
	StoredID string          `json:"stored_id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Created  json.RawMessage `json:"created,omitempty"`
}

// Bootstrap phases: what the last planned call was.
const (
	bootPhaseGetStored = "get_stored"
	bootPhaseList      = "list"
	bootPhaseCreate    = "create"
	bootPhaseRelist    = "relist"
)

// BootstrapStep runs the create-or-adopt sequence Run performs, one Zernio
// call at a time, for a bootstrap whose Zernio calls river-service makes. It
// returns the next call, or done once the profile is persisted. A stored
// profile that still exists is adopted; a stale one (404) is cleared and the
// oldest profile with the tenant's name is adopted, or one is created. ctx must
// carry the tenant.
//
// Errors are the Zernio or settings failure that ended the attempt; the
// caller decides whether it is worth a retry.
func (b *Bootstrapper) BootstrapStep(ctx context.Context, st *BootstrapState, res *Result) (call *Call, done bool, err error) {
	switch st.Phase {
	case "":
		return b.bootStart(ctx, st)
	case bootPhaseGetStored:
		var profile *Profile
		err := Decode(*res, func(c *Client) error {
			var err error
			profile, err = c.GetProfile(ctx, st.StoredID)
			return err
		})
		if err == nil {
			return nil, true, b.persistProfile(ctx, profile)
		}
		if !IsStatus(err, http.StatusNotFound) {
			return nil, false, err
		}
		if err := b.store.Delete(ctx, SettingProfileID); err != nil {
			return nil, false, fmt.Errorf("clear stale profile_id: %w", err)
		}
		return b.bootList(ctx, st)
	case bootPhaseList:
		profiles, err := decodeProfiles(ctx, *res)
		if err != nil {
			return nil, false, err
		}
		if existing := pickOldestByName(profiles, st.Name); existing != nil {
			return nil, true, b.persistProfile(ctx, existing)
		}
		st.Phase = bootPhaseCreate
		c, err := Plan(func(c *Client) error {
			_, err := c.CreateProfile(ctx, st.Name, ManagedProfileDescription)
			return err
		})
		return &c, false, err
	case bootPhaseCreate:
		var created *Profile
		if err := Decode(*res, func(c *Client) error {
			var err error
			created, err = c.CreateProfile(ctx, st.Name, ManagedProfileDescription)
			return err
		}); err != nil {
			return nil, false, err
		}
		raw, err := json.Marshal(created)
		if err != nil {
			return nil, false, err
		}
		st.Created = raw
		// List again: a concurrent bootstrap may have created one too, and
		// every replica must adopt the same (oldest) profile.
		st.Phase = bootPhaseRelist
		c, err := Plan(func(c *Client) error {
			_, err := c.ListProfiles(ctx)
			return err
		})
		return &c, false, err
	case bootPhaseRelist:
		var created Profile
		if err := json.Unmarshal(st.Created, &created); err != nil {
			return nil, false, fmt.Errorf("decode created profile: %w", err)
		}
		chosen := &created
		if profiles, err := decodeProfiles(ctx, *res); err == nil {
			if oldest := pickOldestByName(profiles, st.Name); oldest != nil {
				chosen = oldest
			}
		}
		return nil, true, b.persistProfile(ctx, chosen)
	default:
		return nil, false, fmt.Errorf("zernio: unknown bootstrap phase %q", st.Phase)
	}
}

func (b *Bootstrapper) bootStart(ctx context.Context, st *BootstrapState) (*Call, bool, error) {
	stored, found, err := b.store.Get(ctx, SettingProfileID)
	if err != nil {
		return nil, false, fmt.Errorf("read setting: %w", err)
	}
	if found && stored != "" {
		st.Phase, st.StoredID = bootPhaseGetStored, stored
		c, err := Plan(func(c *Client) error {
			_, err := c.GetProfile(ctx, stored)
			return err
		})
		return &c, false, err
	}
	return b.bootList(ctx, st)
}

func (b *Bootstrapper) bootList(ctx context.Context, st *BootstrapState) (*Call, bool, error) {
	st.Phase, st.Name = bootPhaseList, b.profileName(ctx)
	c, err := Plan(func(c *Client) error {
		_, err := c.ListProfiles(ctx)
		return err
	})
	return &c, false, err
}

func decodeProfiles(ctx context.Context, res Result) ([]Profile, error) {
	var profiles []Profile
	err := Decode(res, func(c *Client) error {
		var err error
		profiles, err = c.ListProfiles(ctx)
		return err
	})
	return profiles, err
}
