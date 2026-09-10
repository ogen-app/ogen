package zernio

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// remoteAccount builds a minimal Account suitable for table tests.
func remoteAccount(id, platform, username string, isActive bool) Account {
	return Account{
		ID:        id,
		ProfileID: "p1",
		Platform:  platform,
		Username:  username,
		IsActive:  isActive,
		CreatedAt: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		Raw:       json.RawMessage(`{"_id":"` + id + `"}`),
	}
}

// localRow builds a SocialAccount as the local DB would have it.
func localRow(id, platform, username string, isActive bool, deletedAt *time.Time) models.SocialAccount {
	return models.SocialAccount{
		ID:           id,
		Platform:     platform,
		ProfileID:    "p1",
		Username:     username,
		IsActive:     isActive,
		ConnectedAt:  time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		LastSyncedAt: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		DeletedAt:    deletedAt,
	}
}

func changeIDs(plan ReconcilePlan, change ReconcileChange) []string {
	var out []string
	for _, c := range plan.Changes {
		if c.Change == change {
			out = append(out, c.Account.ID)
		}
	}
	slices.Sort(out)
	return out
}

func TestReconcile(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	deletedAt := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name           string
		remote         []Account
		local          []models.SocialAccount
		wantAttach     []string
		wantUpdate     []string
		wantDisconnect []string
		wantRevive     []string
		wantSoftDelete []string
		wantUpserts    int
	}{
		{
			name:        "empty local plus N remote yields N attaches",
			remote:      []Account{remoteAccount("a", "linkedin", "alice", true), remoteAccount("b", "twitter", "bob", true)},
			local:       nil,
			wantAttach:  []string{"a", "b"},
			wantUpserts: 2,
		},
		{
			name:           "N local plus empty remote yields N disconnects",
			remote:         nil,
			local:          []models.SocialAccount{localRow("a", "linkedin", "alice", true, nil), localRow("b", "twitter", "bob", true, nil)},
			wantDisconnect: []string{"a", "b"},
			wantSoftDelete: []string{"a", "b"},
			wantUpserts:    0,
		},
		{
			name:        "overlap with username change yields update",
			remote:      []Account{remoteAccount("a", "linkedin", "alice2", true)},
			local:       []models.SocialAccount{localRow("a", "linkedin", "alice", true, nil)},
			wantUpdate:  []string{"a"},
			wantUpserts: 1,
		},
		{
			name:        "overlap with no field change yields silent upsert",
			remote:      []Account{remoteAccount("a", "linkedin", "alice", true)},
			local:       []models.SocialAccount{localRow("a", "linkedin", "alice", true, nil)},
			wantUpserts: 1, // silent refresh of raw_json + last_synced_at
		},
		{
			name:        "soft-deleted local now back in remote yields revive",
			remote:      []Account{remoteAccount("a", "linkedin", "alice", true)},
			local:       []models.SocialAccount{localRow("a", "linkedin", "alice", true, &deletedAt)},
			wantRevive:  []string{"a"},
			wantUpserts: 1,
		},
		{
			name:           "mix of attach update disconnect",
			remote:         []Account{remoteAccount("a", "linkedin", "alice2", true), remoteAccount("c", "youtube", "carol", true)},
			local:          []models.SocialAccount{localRow("a", "linkedin", "alice", true, nil), localRow("b", "twitter", "bob", true, nil)},
			wantAttach:     []string{"c"},
			wantUpdate:     []string{"a"},
			wantDisconnect: []string{"b"},
			wantSoftDelete: []string{"b"},
			wantUpserts:    2,
		},
		{
			name:        "is_active toggle counts as update",
			remote:      []Account{remoteAccount("a", "linkedin", "alice", false)},
			local:       []models.SocialAccount{localRow("a", "linkedin", "alice", true, nil)},
			wantUpdate:  []string{"a"},
			wantUpserts: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := Reconcile(tt.remote, tt.local, "p1", now)

			if got := changeIDs(plan, ChangeAttached); !equalStrings(got, tt.wantAttach) {
				t.Errorf("attached: got %v want %v", got, tt.wantAttach)
			}
			if got := changeIDs(plan, ChangeUpdated); !equalStrings(got, tt.wantUpdate) {
				t.Errorf("updated: got %v want %v", got, tt.wantUpdate)
			}
			if got := changeIDs(plan, ChangeDisconnected); !equalStrings(got, tt.wantDisconnect) {
				t.Errorf("disconnected: got %v want %v", got, tt.wantDisconnect)
			}
			if got := changeIDs(plan, ChangeRevived); !equalStrings(got, tt.wantRevive) {
				t.Errorf("revived: got %v want %v", got, tt.wantRevive)
			}

			gotSoftDelete := append([]string{}, plan.SoftDeleteIDs...)
			slices.Sort(gotSoftDelete)
			if !equalStrings(gotSoftDelete, tt.wantSoftDelete) {
				t.Errorf("soft delete: got %v want %v", gotSoftDelete, tt.wantSoftDelete)
			}

			if len(plan.Upserts) != tt.wantUpserts {
				t.Errorf("upserts: got %d want %d", len(plan.Upserts), tt.wantUpserts)
			}

			// Every upsert should carry the supplied `now` as its
			// LastSyncedAt — that's the contract the worker depends on.
			for _, u := range plan.Upserts {
				if !u.LastSyncedAt.Equal(now) {
					t.Errorf("upsert %s LastSyncedAt: got %v want %v", u.ID, u.LastSyncedAt, now)
				}
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
