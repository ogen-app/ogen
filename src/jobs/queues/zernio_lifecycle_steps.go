package queues

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// Child kinds river-service runs for ogen's sweeps. They exist only between
// the two services: nothing in ogen enqueues them.
const (
	ExpiringConnectionsTenantKind = "detect_expiring_connections_tenant"
)

// teardownLeaseTTL is how long a teardown step holds off a restore. Each step
// renews it, and a step takes seconds, so a stalled teardown frees the tenant
// within this.
const teardownLeaseTTL = 2 * time.Minute

// TeardownLease guards a remote Zernio profile teardown against a concurrent
// restore. *repository.TenantTeardownFence implements it.
type TeardownLease interface {
	BeginTeardownLease(ctx context.Context, tenantID string, ttl time.Duration) (status string, found bool, err error)
	RenewTeardownLease(ctx context.Context, tenantID string, ttl time.Duration) (bool, error)
	EndTeardownLease(ctx context.Context, tenantID string) error
}

// zernioAuthFailed reports a rejected Zernio key: retrying won't help.
func zernioAuthFailed(err error) bool {
	return zernio.IsStatus(err, http.StatusUnauthorized) || zernio.IsStatus(err, http.StatusForbidden)
}

func (z *JobSteps) registerZernioLifecycle() {
	z.kinds[BootstrapZernioProfileQueue] = z.bootstrap
	z.kinds[TeardownZernioProfileQueue] = z.teardown
	z.kinds[DetectExpiringConnectionsQueue] = z.expiringSweep
	z.kinds[ExpiringConnectionsTenantKind] = z.expiringTenant
}

// --- bootstrap_zernio_profile ---------------------------------------------

func (z *JobSteps) bootstrap(ctx context.Context, in StepInput) (StepOutput, error) {
	var task BootstrapZernioProfileTask
	if err := json.Unmarshal(in.Args, &task); err != nil || task.TenantID == "" {
		return done("skipped_bad_args")
	}
	if z.deps.ProfileBootstrapper == nil {
		return done("skipped_unwired")
	}
	ctx = tenantctx.With(ctx, task.TenantID)
	var st zernio.BootstrapState
	if err := decodeState(in.State, &st); err != nil {
		return done("skipped_bad_state")
	}
	if st.Phase == "" {
		// A tenant frozen between signup and now gets no profile.
		active, err := tenantIsActive(ctx, z.deps.Tenants, task.TenantID)
		if err != nil {
			return StepOutput{}, err
		}
		if !active {
			return done("skipped_inactive")
		}
	}
	var res *zernio.Result
	if in.Result != nil {
		r := zernioResult(in.Result)
		res = &r
	}
	if zernioDisabled(in.Result) {
		return done("skipped_zernio_disabled")
	}
	call, finished, err := z.deps.ProfileBootstrapper.BootstrapStep(ctx, &st, res)
	switch {
	case zernioAuthFailed(err):
		slog.WarnContext(ctx, "bootstrap gave up: zernio rejected the key", logging.AttrComponent, "jobs.bootstrap_zernio_profile", logging.AttrError, err)
		return done("gave_up_auth")
	case err != nil:
		return retry("bootstrap", err)
	case finished:
		slog.InfoContext(ctx, "bootstrap ok", logging.AttrComponent, "jobs.bootstrap_zernio_profile")
		return done("bootstrapped")
	default:
		return nextZernio(st, *call)
	}
}

// --- teardown_zernio_profile ----------------------------------------------

type teardownState struct {
	Phase     string   `json:"phase"`
	ProfileID string   `json:"profile_id"`
	Accounts  []string `json:"accounts,omitempty"`
	Next      int      `json:"next,omitzero"`
}

const (
	tdPhaseList          = "list"
	tdPhaseDeleteAccount = "delete_account"
	tdPhaseDeleteProfile = "delete_profile"
)

func (z *JobSteps) teardown(ctx context.Context, in StepInput) (StepOutput, error) {
	var task TeardownZernioProfileTask
	if err := json.Unmarshal(in.Args, &task); err != nil || task.TenantID == "" {
		return done("skipped_bad_args")
	}
	if z.deps.TeardownLease == nil || z.deps.AnalyticsSettings == nil {
		return done("skipped_unwired")
	}
	tid := task.TenantID
	ctx = tenantctx.With(ctx, tid)
	var st teardownState
	if err := decodeState(in.State, &st); err != nil {
		return done("skipped_bad_state")
	}
	if st.Phase == "" {
		return z.teardownStart(ctx, tid)
	}

	// Every destructive call is made under a fresh lease; if the lease lapsed
	// and the workspace was restored meanwhile, stop.
	torn, err := z.deps.TeardownLease.RenewTeardownLease(ctx, tid, teardownLeaseTTL)
	if err != nil {
		return StepOutput{}, err
	}
	if !torn {
		return done("aborted_restored")
	}
	if zernioDisabled(in.Result) {
		_ = z.deps.TeardownLease.EndTeardownLease(ctx, tid)
		return done("skipped_zernio_disabled")
	}
	out, err := z.teardownAdvance(ctx, &st, zernioResult(in.Result))
	if zernioAuthFailed(err) {
		slog.WarnContext(ctx, "teardown gave up: zernio rejected the key", logging.AttrComponent, "jobs.teardown_zernio_profile", "profile_id", st.ProfileID, logging.AttrError, err)
		_ = z.deps.TeardownLease.EndTeardownLease(ctx, tid)
		return done("gave_up_auth")
	}
	if err != nil {
		return retry("teardown", err)
	}
	return out, nil
}

func (z *JobSteps) teardownStart(ctx context.Context, tid string) (StepOutput, error) {
	status, _, err := z.deps.TeardownLease.BeginTeardownLease(ctx, tid, teardownLeaseTTL)
	if err != nil {
		return StepOutput{}, err
	}
	if status == models.TenantStatusActive {
		return done("skipped_active")
	}
	profileID, found, err := z.deps.AnalyticsSettings.Get(ctx, zernio.SettingProfileID)
	if err != nil {
		return StepOutput{}, err
	}
	if !found || profileID == "" {
		return z.teardownFinish(ctx, tid, "noop_no_profile")
	}
	call, err := zernio.Plan(func(c *zernio.Client) error {
		_, err := c.ListAccounts(ctx, profileID)
		return err
	})
	if err != nil {
		return StepOutput{}, err
	}
	return nextZernio(teardownState{Phase: tdPhaseList, ProfileID: profileID}, call)
}

// teardownAdvance handles the result of the last call: disconnect every
// account, then delete the profile, then clear the local pointer. A 404 at
// any point means that part is already gone.
func (z *JobSteps) teardownAdvance(ctx context.Context, st *teardownState, res zernio.Result) (StepOutput, error) {
	tid, _ := tenantctx.From(ctx)
	switch st.Phase {
	case tdPhaseList:
		var accounts []zernio.Account
		err := zernio.Decode(res, func(c *zernio.Client) error {
			var err error
			accounts, err = c.ListAccounts(ctx, st.ProfileID)
			return err
		})
		if zernio.IsStatus(err, http.StatusNotFound) {
			return z.teardownFinish(ctx, tid, "torn_down_already_gone")
		}
		if err != nil {
			return StepOutput{}, err
		}
		for _, a := range accounts {
			st.Accounts = append(st.Accounts, a.ID)
		}
		return z.teardownNextDelete(ctx, st)
	case tdPhaseDeleteAccount:
		id := st.Accounts[st.Next]
		if err := zernio.Decode(res, func(c *zernio.Client) error { return c.DeleteAccount(ctx, id) }); err != nil && !zernio.IsStatus(err, http.StatusNotFound) {
			return StepOutput{}, fmt.Errorf("disconnect account %s: %w", id, err)
		}
		st.Next++
		return z.teardownNextDelete(ctx, st)
	case tdPhaseDeleteProfile:
		if err := zernio.Decode(res, func(c *zernio.Client) error { return c.DeleteProfile(ctx, st.ProfileID) }); err != nil && !zernio.IsStatus(err, http.StatusNotFound) {
			return StepOutput{}, fmt.Errorf("delete profile: %w", err)
		}
		return z.teardownFinish(ctx, tid, "torn_down")
	default:
		return StepOutput{}, fmt.Errorf("unknown teardown phase %q", st.Phase)
	}
}

// teardownNextDelete disconnects the next account, or deletes the profile once
// none are left: Zernio refuses to delete a profile that still has accounts.
func (z *JobSteps) teardownNextDelete(ctx context.Context, st *teardownState) (StepOutput, error) {
	var call zernio.Call
	var err error
	if st.Next < len(st.Accounts) {
		id := st.Accounts[st.Next]
		st.Phase = tdPhaseDeleteAccount
		call, err = zernio.Plan(func(c *zernio.Client) error { return c.DeleteAccount(ctx, id) })
	} else {
		st.Phase = tdPhaseDeleteProfile
		call, err = zernio.Plan(func(c *zernio.Client) error { return c.DeleteProfile(ctx, st.ProfileID) })
	}
	if err != nil {
		return StepOutput{}, err
	}
	return nextZernio(st, call)
}

// teardownFinish clears the local profile keys and releases the lease. A
// failure to clear must fail the step, or a dangling profile id survives.
func (z *JobSteps) teardownFinish(ctx context.Context, tid, outcome string) (StepOutput, error) {
	for _, k := range []string{zernio.SettingProfileID, zernio.SettingProfileName, zernio.SettingProfileCreatedAt, zernio.SettingProfileMeta} {
		if err := z.deps.AnalyticsSettings.Delete(ctx, k); err != nil {
			return StepOutput{}, fmt.Errorf("clear %s: %w", k, err)
		}
	}
	if err := z.deps.TeardownLease.EndTeardownLease(ctx, tid); err != nil {
		return StepOutput{}, err
	}
	slog.InfoContext(ctx, "teardown finished", logging.AttrComponent, "jobs.teardown_zernio_profile", "outcome", outcome)
	return done(outcome)
}

// --- detect_expiring_connections ------------------------------------------

type expiringTenantArgs struct {
	TenantID  string `json:"tenant_id"`
	ProfileID string `json:"profile_id"`
}

// expiringSweep fans the connection-health sweep out into one job per tenant
// with a Zernio profile, so each runs on its own and a slow tenant holds up
// nobody.
func (z *JobSteps) expiringSweep(ctx context.Context, _ StepInput) (StepOutput, error) {
	repo := z.deps.Zernio.SocialAccountRepo
	if repo == nil {
		return done("skipped_unwired")
	}
	pairs, err := repo.ListActiveTenantProfiles(tenantctx.WithSystem(ctx))
	if err != nil {
		return StepOutput{}, err
	}
	children := make([]ChildJob, 0, len(pairs))
	for _, tp := range pairs {
		if tp.ProfileID == "" {
			continue
		}
		raw, err := json.Marshal(expiringTenantArgs{TenantID: tp.TenantID, ProfileID: tp.ProfileID})
		if err != nil {
			return StepOutput{}, err
		}
		children = append(children, ChildJob{Kind: ExpiringConnectionsTenantKind, Args: raw})
	}
	return done(fmt.Sprintf("fanned_out_%d", len(children)), children...)
}

// expiringTenant checks one tenant's account health: fetch it from Zernio,
// then record it and notify owners as the in-process sweep does.
func (z *JobSteps) expiringTenant(ctx context.Context, in StepInput) (StepOutput, error) {
	var args expiringTenantArgs
	if err := json.Unmarshal(in.Args, &args); err != nil || args.TenantID == "" || args.ProfileID == "" {
		return done("skipped_bad_args")
	}
	ctx = tenantctx.With(ctx, args.TenantID)
	if in.Result == nil {
		call, err := zernio.Plan(func(c *zernio.Client) error {
			_, err := c.GetAccountsHealth(ctx, args.ProfileID)
			return err
		})
		if err != nil {
			return StepOutput{}, err
		}
		return nextZernio(struct{}{}, call)
	}
	if zernioDisabled(in.Result) {
		return done("skipped_zernio_disabled")
	}
	var healths []zernio.AccountHealth
	err := zernio.Decode(zernioResult(in.Result), func(c *zernio.Client) error {
		var err error
		healths, err = c.GetAccountsHealth(ctx, args.ProfileID)
		return err
	})
	if zernioAuthFailed(err) {
		return done("gave_up_auth")
	}
	if err != nil {
		return retry("account health", err)
	}
	p := newDetectExpiringConnections(z.deps)
	res, err := p.applyHealth(ctx, args.TenantID, args.ProfileID, healths, time.Now().UTC())
	if err != nil {
		return StepOutput{}, err
	}
	return done(fmt.Sprintf("checked_%d_notified_%d", res.checked, res.notified))
}
