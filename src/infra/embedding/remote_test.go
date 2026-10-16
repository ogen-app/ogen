package embedding

import (
	"encoding/json"
	"testing"

	"google.golang.org/genai"
)

func TestPlanBatchMatchesTheEmbedderConfig(t *testing.T) {
	dims := int32(3072)
	req, err := PlanBatch("googleai/gemini-embedding-2", []string{"a", "b"}, &genai.EmbedContentConfig{TaskType: "RETRIEVAL_DOCUMENT", OutputDimensionality: &dims})
	if err != nil {
		t.Fatal(err)
	}
	if req.Path != "/v1beta/models/gemini-embedding-2:batchEmbedContents" {
		t.Errorf("path = %s", req.Path)
	}
	var body batchBody
	if err := json.Unmarshal(req.Body, &body); err != nil || len(body.Requests) != 2 {
		t.Fatalf("body = %s", req.Body)
	}
	r := body.Requests[1]
	if r.Model != "models/gemini-embedding-2" || r.Content.Parts[0].Text != "b" || r.TaskType != "RETRIEVAL_DOCUMENT" || *r.OutputDimensionality != 3072 {
		t.Errorf("request = %+v", r)
	}
}

func TestDecodeBatch(t *testing.T) {
	vecs, err := DecodeBatch(200, []byte(`{"embeddings":[{"values":[1,2]},{"values":[3,4]}]}`), 2)
	if err != nil || len(vecs) != 2 || vecs[1][1] != 4 {
		t.Fatalf("vecs = %v, %v", vecs, err)
	}
	if _, err := DecodeBatch(200, []byte(`{"embeddings":[{"values":[1]}]}`), 2); err == nil {
		t.Error("a short batch decoded")
	}
	if _, err := DecodeBatch(429, []byte(`{"error":{"message":"quota"}}`), 1); err == nil || err.Error() != "embedding: gemini 429: quota" {
		t.Errorf("429 = %v", err)
	}
}
