package packs

import "github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"

// ideaRetry reads a bulk idea change: the ideas it couldn't find or change,
// and the ones a stop on an error left, which with sets on the retried call.
func ideaRetry[In any](with func(In, []string) In) assistantkit.Retry[In, *BulkIdeasOutput] {
	return func(in In, out *BulkIdeasOutput) (In, []assistantkit.NotMade, bool) {
		var notMade []assistantkit.NotMade
		for _, id := range out.Failed {
			notMade = append(notMade, assistantkit.NotMade{ID: id, Reason: "it no longer exists or can't be changed"})
		}
		for _, id := range out.left {
			notMade = append(notMade, assistantkit.NotMade{ID: id, Reason: out.Stopped})
		}
		return with(in, out.left), notMade, len(out.left) > 0
	}
}
