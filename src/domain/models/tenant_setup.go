package models

import (
	"regexp"
	"time"
)

// Setup statuses of a workspace. The set is closed and CHECK-enforced.
const (
	SetupStatusPending = "pending"
	SetupStatusDone    = "done"
	SetupStatusSkipped = "skipped"
)

// Setup roles: the workspace is the person's own brand, or a client's.
const (
	SetupRoleOwn    = "own"
	SetupRoleClient = "client"
)

// TenantSetup is where a workspace is in setup after signup. Setup belongs to
// the workspace, not a person: a teammate invited later finds it done.
type TenantSetup struct {
	Status string `json:"status" enums:"pending,done,skipped"`
	// Step is the step to open on; Reached is the furthest one reached. The
	// client names its steps.
	Step    string `json:"step"`
	Reached string `json:"reached"`
	// Role is own or client; empty until chosen.
	Role string `json:"role" enums:",own,client"`
	// RunID is the brand read setup started, so a reload finds it again.
	RunID      *string    `json:"runId"      extensions:"x-nullable"`
	UpdatedAt  *time.Time `json:"updatedAt"  extensions:"x-nullable"`
	FinishedBy *string    `json:"finishedBy" extensions:"x-nullable"`
	FinishedAt *time.Time `json:"finishedAt" extensions:"x-nullable"`
}

// SetupState is the workspace's setup, nil for a workspace made before setup
// existed.
func (t *Tenant) SetupState() *TenantSetup {
	if t == nil || t.SetupStatus == nil {
		return nil
	}
	return &TenantSetup{
		Status: *t.SetupStatus, Step: t.SetupStep, Reached: t.SetupReached, Role: t.SetupRole,
		RunID: t.SetupRunID, UpdatedAt: t.SetupUpdatedAt, FinishedBy: t.SetupFinishedBy, FinishedAt: t.SetupFinishedAt,
	}
}

// StartSetup marks a new workspace as owing its setup.
func (t *Tenant) StartSetup(now time.Time) {
	t.SetupStatus, t.SetupUpdatedAt = new(SetupStatusPending), &now
}

// SetupUpdate is a write of the setup state.
type SetupUpdate struct {
	Status  string
	Step    string
	Reached string
	Role    string
	RunID   *string
}

// InvalidSetupError is a setup write that breaks the rules; the message says which.
type InvalidSetupError struct{ Msg string }

func (e *InvalidSetupError) Error() string { return e.Msg }

// setupStepRe is what a client step name may look like.
var setupStepRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// ApplySetup writes u onto the workspace. Finishing or skipping stamps who and
// when; going back to pending clears them.
func (t *Tenant) ApplySetup(u SetupUpdate, userID string, now time.Time) error {
	if err := u.validate(); err != nil {
		return err
	}
	t.SetupStatus, t.SetupStep, t.SetupReached, t.SetupRole, t.SetupRunID = &u.Status, u.Step, u.Reached, u.Role, u.RunID
	t.SetupUpdatedAt = &now
	if u.Status == SetupStatusPending {
		t.SetupFinishedBy, t.SetupFinishedAt = nil, nil
		return nil
	}
	if t.SetupFinishedAt == nil {
		t.SetupFinishedBy, t.SetupFinishedAt = &userID, &now
	}
	return nil
}

func (u SetupUpdate) validate() error {
	switch u.Status {
	case SetupStatusPending, SetupStatusDone, SetupStatusSkipped:
	default:
		return &InvalidSetupError{Msg: "status is pending, done or skipped"}
	}
	for _, s := range []string{u.Step, u.Reached} {
		if s != "" && !setupStepRe.MatchString(s) {
			return &InvalidSetupError{Msg: "a step is a lower-case name of at most 32 characters"}
		}
	}
	switch u.Role {
	case "", SetupRoleOwn, SetupRoleClient:
	default:
		return &InvalidSetupError{Msg: "role is own or client"}
	}
	if u.RunID != nil && (*u.RunID == "" || len(*u.RunID) > 64) {
		return &InvalidSetupError{Msg: "runId is a run id"}
	}
	return nil
}
