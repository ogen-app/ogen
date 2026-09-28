// Package accountselect is the single source of truth for the
// account-selection rule: which connected same-platform account a post publishes
// to. The rule — an explicit post.social_account_id must be connected and
// on-platform; otherwise the platform's sole connected account is auto-selected,
// zero is "none", two-or-more is "ambiguous" — is shared by the schedule
// pre-flight gate, the submit worker's authoritative resolver, and
// external-post verification, so they cannot drift. This package
// classifies once; each caller maps the Outcome to its own behaviour (the gate
// defers the 0/1 cases to the worker, the worker terminal-fails them, the verify
// endpoint writes an HTTP error).
package accountselect

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// Outcome is the classification of a post's account selection under the rule.
type Outcome int

const (
	// Resolved: an account was determined — Result.AccountID / Result.Account set.
	Resolved Outcome = iota
	// Unavailable: an explicit account was named but is not connected.
	Unavailable
	// PlatformMismatch: the explicit account is connected but on another platform.
	// Result.Account carries it (for messages).
	PlatformMismatch
	// NoAccount: no explicit choice and zero connected accounts on the platform.
	NoAccount
	// Ambiguous: no explicit choice and two-or-more connected accounts —
	// Result.Candidates lists them.
	Ambiguous
)

// Candidate identifies one connected same-platform account the caller may
// surface for the user to choose from.
type Candidate struct {
	ID          string
	Username    string
	DisplayName string
}

// Result carries the classification plus the data each caller needs to act or
// build its message/response.
type Result struct {
	Outcome    Outcome
	AccountID  string                // set when Resolved
	Account    *models.SocialAccount // set when Resolved or PlatformMismatch
	Candidates []Candidate           // set when Ambiguous
}

// Resolve applies the rule for publishing post on platform (a Zernio platform
// id) under profileID. The error return is a real repository failure only; every
// rule outcome — including the failure modes — is carried in Result.Outcome so
// callers can map it without inspecting errors.
func Resolve(ctx context.Context, accounts repository.SocialAccountRepository, profileID string, post *models.Post, platform string) (Result, error) {
	// Explicit selection wins: it must be connected and on the right platform.
	if post.SocialAccountID != "" {
		acc, err := accounts.GetActive(ctx, profileID, post.SocialAccountID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Result{Outcome: Unavailable}, nil
			}
			return Result{}, err
		}
		if acc.Platform != platform {
			return Result{Outcome: PlatformMismatch, Account: acc}, nil
		}
		return Result{Outcome: Resolved, AccountID: acc.ID, Account: acc}, nil
	}

	// No explicit choice: auto-select the sole account, else 0 → NoAccount,
	// 2+ → Ambiguous (Zernio's own disambiguation rule).
	list, err := accounts.ListActiveByPlatform(ctx, profileID, platform)
	if err != nil {
		return Result{}, err
	}
	switch len(list) {
	case 0:
		return Result{Outcome: NoAccount}, nil
	case 1:
		return Result{Outcome: Resolved, AccountID: list[0].ID, Account: &list[0]}, nil
	default:
		cands := make([]Candidate, 0, len(list))
		for i := range list {
			cands = append(cands, Candidate{ID: list[i].ID, Username: list[i].Username, DisplayName: list[i].DisplayName})
		}
		return Result{Outcome: Ambiguous, Candidates: cands}, nil
	}
}
