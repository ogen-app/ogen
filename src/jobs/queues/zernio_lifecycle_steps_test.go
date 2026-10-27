package queues_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// scriptedZernio answers planned calls by "METHOD path", and records them.
type scriptedZernio struct {
	replies map[string]zernio.Result
	calls   []string
}

func (s *scriptedZernio) answer(c queues.HTTPCall) queues.StepResult {
	key := c.Method + " " + c.Path
	s.calls = append(s.calls, key)
	if r, ok := s.replies[key]; ok {
		return queues.StepResult{Status: r.Status, Body: r.Body, Err: r.Err}
	}
	return queues.StepResult{Status: 500, Body: []byte(`{"error":"unscripted ` + key + `"}`)}
}

// runSteps drives one job to the end the way river-service does.
func runSteps(t *testing.T, steps *queues.JobSteps, kind string, args any, z *scriptedZernio) queues.StepOutput {
	t.Helper()
	raw, _ := json.Marshal(args)
	in := queues.StepInput{Kind: kind, Args: raw}
	for range 20 {
		out, err := steps.Step(context.Background(), in)
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if out.HTTP == nil {
			return out
		}
		if out.HTTP.Upstream != queues.UpstreamZernio {
			t.Fatalf("call to %q, want zernio", out.HTTP.Upstream)
		}
		res := z.answer(*out.HTTP)
		in.State, in.Result = out.State, &res
	}
	t.Fatal("job did not finish in 20 steps")
	return queues.StepOutput{}
}

type activeTenants struct{ status string }

func (a activeTenants) GetStatus(context.Context, string) (string, error) { return a.status, nil }

func profileJSON(id, name, created string) string {
	return fmt.Sprintf(`{"_id":%q,"name":%q,"createdAt":%q}`, id, name, created)
}

func TestBootstrapStepsCreateAndAdoptTheOldest(t *testing.T) {
	settings := newFakeSettings()
	boot := zernio.NewBootstrapper(nil, settings, "dev")
	steps := queues.NewJobSteps(queues.Deps{ProfileBootstrapper: boot, Tenants: activeTenants{models.TenantStatusActive}})
	name := zernio.ManagedProfileNameFor("dev", "t1")
	z := &scriptedZernio{replies: map[string]zernio.Result{
		"POST /profiles": {Status: 201, Body: []byte(`{"profile":` + profileJSON("p-new", name, "2026-10-14T10:00:00Z") + `}`)},
	}}
	// First list: none yet. After create, the relist shows an older twin made
	// by a concurrent bootstrap, which must win.
	lists := []string{`{"profiles":[]}`, `{"profiles":[` + profileJSON("p-new", name, "2026-10-14T10:00:00Z") + `,` + profileJSON("p-old", name, "2026-10-14T09:00:00Z") + `]}`}
	stepsWithLists := &listSequence{scriptedZernio: z, lists: lists}

	out := runStepsWith(t, steps, queues.BootstrapZernioProfileQueue, map[string]string{"tenant_id": "t1"}, stepsWithLists.answer)
	if out.Done == nil || out.Done.Outcome != "bootstrapped" {
		t.Fatalf("outcome = %+v", out)
	}
	ctx := tenantctx.With(context.Background(), "t1")
	if id, _, _ := settings.Get(ctx, zernio.SettingProfileID); id != "p-old" {
		t.Errorf("stored profile = %q, want the oldest p-old", id)
	}
}

func TestBootstrapStepsAdoptAStoredProfile(t *testing.T) {
	settings := newFakeSettings()
	ctx := tenantctx.With(context.Background(), "t1")
	_ = settings.Set(ctx, zernio.SettingProfileID, "p-1")
	steps := queues.NewJobSteps(queues.Deps{ProfileBootstrapper: zernio.NewBootstrapper(nil, settings, "dev"), Tenants: activeTenants{models.TenantStatusActive}})
	z := &scriptedZernio{replies: map[string]zernio.Result{
		"GET /profiles/p-1": {Status: 200, Body: []byte(`{"profile":` + profileJSON("p-1", "x", "2026-10-01T00:00:00Z") + `}`)},
	}}
	out := runSteps(t, steps, queues.BootstrapZernioProfileQueue, map[string]string{"tenant_id": "t1"}, z)
	if out.Done == nil || out.Done.Outcome != "bootstrapped" || len(z.calls) != 1 {
		t.Fatalf("outcome %+v after calls %v", out, z.calls)
	}
}

func TestBootstrapStepsOutcomes(t *testing.T) {
	steps := queues.NewJobSteps(queues.Deps{ProfileBootstrapper: zernio.NewBootstrapper(nil, newFakeSettings(), "dev"), Tenants: activeTenants{models.TenantStatusSuspended}})
	if out := runSteps(t, steps, queues.BootstrapZernioProfileQueue, map[string]string{"tenant_id": "t1"}, &scriptedZernio{}); out.Done == nil || out.Done.Outcome != "skipped_inactive" {
		t.Errorf("inactive tenant: %+v", out)
	}

	steps = queues.NewJobSteps(queues.Deps{ProfileBootstrapper: zernio.NewBootstrapper(nil, newFakeSettings(), "dev"), Tenants: activeTenants{models.TenantStatusActive}})
	z := &scriptedZernio{replies: map[string]zernio.Result{"GET /profiles": {Status: 401, Body: []byte(`{"error":"bad key"}`)}}}
	if out := runSteps(t, steps, queues.BootstrapZernioProfileQueue, map[string]string{"tenant_id": "t1"}, z); out.Done == nil || out.Done.Outcome != "gave_up_auth" {
		t.Errorf("rejected key: %+v", out)
	}

	z = &scriptedZernio{replies: map[string]zernio.Result{"GET /profiles": {Status: 503}}}
	if out := runSteps(t, steps, queues.BootstrapZernioProfileQueue, map[string]string{"tenant_id": "t1"}, z); out.Retry == nil {
		t.Errorf("zernio down: %+v, want a retry", out)
	}
}

// listSequence answers successive GET /profiles calls from lists.
type listSequence struct {
	*scriptedZernio
	lists []string
}

func (l *listSequence) answer(c queues.HTTPCall) queues.StepResult {
	if c.Method == "GET" && c.Path == "/profiles" && len(l.lists) > 0 {
		body := l.lists[0]
		l.lists = l.lists[1:]
		l.calls = append(l.calls, "GET /profiles")
		return queues.StepResult{Status: 200, Body: []byte(body)}
	}
	return l.scriptedZernio.answer(c)
}

func runStepsWith(t *testing.T, steps *queues.JobSteps, kind string, args any, answer func(queues.HTTPCall) queues.StepResult) queues.StepOutput {
	t.Helper()
	raw, _ := json.Marshal(args)
	in := queues.StepInput{Kind: kind, Args: raw}
	for range 20 {
		out, err := steps.Step(context.Background(), in)
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if out.HTTP == nil {
			return out
		}
		res := answer(*out.HTTP)
		in.State, in.Result = out.State, &res
	}
	t.Fatal("job did not finish in 20 steps")
	return queues.StepOutput{}
}

// fakeLease is an in-memory teardown lease on one tenant.
type fakeLease struct {
	status  string
	held    bool
	renewed int
}

func (f *fakeLease) BeginTeardownLease(context.Context, string, time.Duration) (string, bool, error) {
	if f.status != models.TenantStatusActive {
		f.held = true
	}
	return f.status, true, nil
}

func (f *fakeLease) RenewTeardownLease(context.Context, string, time.Duration) (bool, error) {
	f.renewed++
	return f.status != models.TenantStatusActive, nil
}

func (f *fakeLease) EndTeardownLease(context.Context, string) error {
	f.held = false
	return nil
}

func TestTeardownStepsDisconnectThenDelete(t *testing.T) {
	settings := newFakeSettings()
	ctx := tenantctx.With(context.Background(), "t1")
	_ = settings.Set(ctx, zernio.SettingProfileID, "p-1")
	_ = settings.Set(ctx, zernio.SettingProfileName, "ogen-dev-t1")
	lease := &fakeLease{status: models.TenantStatusDeleted}
	steps := queues.NewJobSteps(queues.Deps{TeardownLease: lease, AnalyticsSettings: settings})
	z := &scriptedZernio{replies: map[string]zernio.Result{
		"GET /accounts":        {Status: 200, Body: []byte(`{"accounts":[{"_id":"a1","platform":"x"},{"_id":"a2","platform":"linkedin"}]}`)},
		"DELETE /accounts/a1":  {Status: 200, Body: []byte(`{}`)},
		"DELETE /accounts/a2":  {Status: 404, Body: []byte(`{}`)},
		"DELETE /profiles/p-1": {Status: 200, Body: []byte(`{}`)},
	}}
	out := runSteps(t, steps, queues.TeardownZernioProfileQueue, map[string]string{"tenant_id": "t1"}, z)
	if out.Done == nil || out.Done.Outcome != "torn_down" {
		t.Fatalf("outcome = %+v after %v", out, z.calls)
	}
	want := "GET /accounts,DELETE /accounts/a1,DELETE /accounts/a2,DELETE /profiles/p-1"
	if got := strings.Join(z.calls, ","); got != want {
		t.Errorf("calls = %s, want %s", got, want)
	}
	if id, found, _ := settings.Get(ctx, zernio.SettingProfileID); found || id != "" {
		t.Errorf("profile id still stored: %q", id)
	}
	if lease.held || lease.renewed != 4 {
		t.Errorf("lease held %v, renewed %d times; want released after 4 renewals", lease.held, lease.renewed)
	}
}

func TestTeardownStepsStopForARestoredTenant(t *testing.T) {
	settings := newFakeSettings()
	_ = settings.Set(tenantctx.With(context.Background(), "t1"), zernio.SettingProfileID, "p-1")
	steps := queues.NewJobSteps(queues.Deps{TeardownLease: &fakeLease{status: models.TenantStatusActive}, AnalyticsSettings: settings})
	z := &scriptedZernio{}
	if out := runSteps(t, steps, queues.TeardownZernioProfileQueue, map[string]string{"tenant_id": "t1"}, z); out.Done == nil || out.Done.Outcome != "skipped_active" || len(z.calls) != 0 {
		t.Fatalf("active tenant: %+v, calls %v", out, z.calls)
	}

	// Restored after the teardown started (its lease lapsed): stop before the
	// next destructive call.
	lease := &fakeLease{status: models.TenantStatusDeleted}
	steps = queues.NewJobSteps(queues.Deps{TeardownLease: lease, AnalyticsSettings: settings})
	z = &scriptedZernio{replies: map[string]zernio.Result{"GET /accounts": {Status: 200, Body: []byte(`{"accounts":[{"_id":"a1"}]}`)}}}
	answer := func(c queues.HTTPCall) queues.StepResult {
		lease.status = models.TenantStatusActive
		return z.answer(c)
	}
	out := runStepsWith(t, steps, queues.TeardownZernioProfileQueue, map[string]string{"tenant_id": "t1"}, answer)
	if out.Done == nil || out.Done.Outcome != "aborted_restored" || len(z.calls) != 1 {
		t.Fatalf("restored mid-teardown: %+v, calls %v", out, z.calls)
	}
}

func TestUnknownKindIsReportedAsSuch(t *testing.T) {
	_, err := queues.NewJobSteps(queues.Deps{}).Step(context.Background(), queues.StepInput{Kind: "publish_to_mars"})
	if err == nil || !strings.Contains(err.Error(), "unknown job kind") {
		t.Errorf("err = %v", err)
	}
}

func TestZernioStepsWithNoKeyInOgen(t *testing.T) {
	steps := queues.NewJobSteps(queues.Deps{ProfileBootstrapper: zernio.NewBootstrapper(nil, newFakeSettings(), "dev"), Tenants: activeTenants{models.TenantStatusActive}})
	noKey := func(queues.HTTPCall) queues.StepResult { return queues.StepResult{CredentialMissing: true} }
	if out := runStepsWith(t, steps, queues.BootstrapZernioProfileQueue, map[string]string{"tenant_id": "t1"}, noKey); out.Done == nil || out.Done.Outcome != "skipped_zernio_disabled" {
		t.Errorf("bootstrap with no key: %+v", out)
	}
	settings := newFakeSettings()
	_ = settings.Set(tenantctx.With(context.Background(), "t1"), zernio.SettingProfileID, "p-1")
	lease := &fakeLease{status: models.TenantStatusDeleted}
	steps = queues.NewJobSteps(queues.Deps{TeardownLease: lease, AnalyticsSettings: settings})
	if out := runStepsWith(t, steps, queues.TeardownZernioProfileQueue, map[string]string{"tenant_id": "t1"}, noKey); out.Done == nil || out.Done.Outcome != "skipped_zernio_disabled" || lease.held {
		t.Errorf("teardown with no key: %+v, lease held %v", out, lease.held)
	}
}

func trialTeardownSteps(t *testing.T, billing queues.BillingLiveChecker, status string, accounts *fakeLocalAccounts) (*queues.JobSteps, *fakeSettings, context.Context) {
	t.Helper()
	settings := newFakeSettings()
	ctx := tenantctx.With(context.Background(), "t1")
	_ = settings.Set(ctx, zernio.SettingProfileID, "p-1")
	d := queues.Deps{
		TeardownLease: &fakeLease{status: status}, AnalyticsSettings: settings,
		Tenants: activeTenants{status: status}, BillingLive: billing,
	}
	d.Zernio.SocialAccountRepo = accounts
	return queues.NewJobSteps(d), settings, ctx
}

var trialArgs = map[string]string{"tenant_id": "t1", "reason": queues.TeardownReasonTrialUnpaid}

func TestTrialTeardownStepsReleaseALiveWorkspace(t *testing.T) {
	accounts := &fakeLocalAccounts{active: []string{"s1", "s2"}}
	steps, settings, ctx := trialTeardownSteps(t, fakeBillingLive{}, models.TenantStatusActive, accounts)
	z := &scriptedZernio{replies: map[string]zernio.Result{
		"GET /accounts":        {Status: 200, Body: []byte(`{"accounts":[{"_id":"a1"}]}`)},
		"DELETE /accounts/a1":  {Status: 200, Body: []byte(`{}`)},
		"DELETE /profiles/p-1": {Status: 200, Body: []byte(`{}`)},
	}}
	out := runSteps(t, steps, queues.TeardownZernioProfileQueue, trialArgs, z)
	if out.Done == nil || out.Done.Outcome != "torn_down" {
		t.Fatalf("outcome = %+v after %v", out, z.calls)
	}
	if len(accounts.active) != 0 {
		t.Errorf("local accounts still active: %v", accounts.active)
	}
	if id, found, _ := settings.Get(ctx, zernio.SettingProfileID); found || id != "" {
		t.Errorf("profile id still stored: %q", id)
	}
}

func TestTrialTeardownStepsSpareASubscribedWorkspace(t *testing.T) {
	accounts := &fakeLocalAccounts{active: []string{"s1"}}
	steps, _, _ := trialTeardownSteps(t, fakeBillingLive{live: true}, models.TenantStatusActive, accounts)
	z := &scriptedZernio{}
	if out := runSteps(t, steps, queues.TeardownZernioProfileQueue, trialArgs, z); out.Done == nil || out.Done.Outcome != "skipped_subscribed" || len(z.calls) != 0 {
		t.Fatalf("subscribed: %+v, calls %v", out, z.calls)
	}

	// Suspended meanwhile: nothing is torn down either.
	steps, _, _ = trialTeardownSteps(t, fakeBillingLive{}, models.TenantStatusSuspended, accounts)
	if out := runSteps(t, steps, queues.TeardownZernioProfileQueue, trialArgs, z); out.Done == nil || len(z.calls) != 0 {
		t.Fatalf("suspended: %+v, calls %v", out, z.calls)
	}

	// Subscribed after the list: stop before the first delete.
	billing := &switchingBilling{}
	steps, _, _ = trialTeardownSteps(t, billing, models.TenantStatusActive, accounts)
	z = &scriptedZernio{replies: map[string]zernio.Result{"GET /accounts": {Status: 200, Body: []byte(`{"accounts":[{"_id":"a1"}]}`)}}}
	answer := func(c queues.HTTPCall) queues.StepResult {
		billing.live = true
		return z.answer(c)
	}
	out := runStepsWith(t, steps, queues.TeardownZernioProfileQueue, trialArgs, answer)
	if out.Done == nil || out.Done.Outcome != "skipped_subscribed" || len(z.calls) != 1 || len(accounts.active) != 1 {
		t.Fatalf("subscribed mid-teardown: %+v, calls %v, accounts %v", out, z.calls, accounts.active)
	}
}

type switchingBilling struct{ live bool }

func (b *switchingBilling) HasLiveSubscription(context.Context, string) (bool, error) {
	return b.live, nil
}
