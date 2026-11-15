// Package editors says who changed things: the members behind the last
// editors recorded on posts and assets, by name.
package editors

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// Members lists the workspace's members.
type Members interface {
	List(ctx context.Context) ([]models.User, error)
}

// Tally counts the things changed by each member, most first, then by name,
// or by email for a member with no name.
// editedBy has one entry per thing: its last editor, "" when no member
// edited it. Editors who are no longer members are left out, as is
// everything when the members can't be read: who changed it is a detail,
// never a reason to fail.
func Tally(ctx context.Context, members Members, editedBy []string) []models.ChangedBy {
	counts := map[string]int{}
	for _, id := range editedBy {
		if id != "" {
			counts[id]++
		}
	}
	if len(counts) == 0 || members == nil {
		return nil
	}
	users, err := members.List(ctx)
	if err != nil {
		slog.WarnContext(ctx, "list members to name editors failed", logging.AttrError, err)
		return nil
	}
	var out []models.ChangedBy
	for _, u := range users {
		if n := counts[u.ID]; n > 0 {
			out = append(out, models.ChangedBy{UserID: u.ID, Name: cmp.Or(u.Name, u.Email), Count: n})
		}
	}
	slices.SortFunc(out, func(a, b models.ChangedBy) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// Names words who made a change, as its sentence's subject: "you" for ctx's
// member, the others by name, e.g. "Marta and you". "" when by is empty.
func Names(ctx context.Context, by []models.ChangedBy) string {
	self, _ := models.EditorOf(ctx)
	names := make([]string, 0, len(by))
	you := false
	for _, b := range by {
		if b.UserID == self {
			you = true
			continue
		}
		names = append(names, cmp.Or(b.Name, "a teammate"))
	}
	if you {
		names = append(names, "you")
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
