package flowkit

import (
	"context"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/genkit/jsonstream"
)

// ObjectStream is the outcome of StreamObjects.
type ObjectStream struct {
	// Response is the final response; nil when the stream failed.
	Response *ai.ModelResponse
	// Objects counts the raw objects handed to onObject.
	Objects int
	Chunks  int
	Bytes   int
}

// StreamObjects runs a streaming generation whose output is a JSON array of
// objects and calls onObject with each object's raw text and its position in
// the array as soon as it completes. A non-nil error means the stream broke
// off; the positions already delivered let the caller skip them when it
// recovers the rest with a blocking call.
func StreamObjects(
	ctx context.Context,
	g *genkit.Genkit,
	onObject func(pos int, raw string),
	opts ...ai.GenerateOption,
) (ObjectStream, error) {
	var out ObjectStream
	splitter := jsonstream.NewObjectSplitter()
	for result, err := range genkit.GenerateStream(ctx, g, opts...) {
		if err != nil {
			return out, err
		}
		if result.Done {
			out.Response = result.Response
			return out, nil
		}
		text := chunkText(result.Chunk)
		out.Chunks++
		out.Bytes += len(text)
		for _, raw := range splitter.Push(text) {
			pos := out.Objects
			out.Objects++
			onObject(pos, raw)
		}
	}
	return out, nil
}

// chunkText joins a chunk's text parts. ai.ModelResponseChunk.Text returns a
// lone part's text whatever its kind, which would feed a thinking delta into
// the JSON stream.
func chunkText(c *ai.ModelResponseChunk) string {
	if c == nil {
		return ""
	}
	var text string
	for _, p := range c.Content {
		if p.IsText() {
			text += p.Text
		}
	}
	return text
}
