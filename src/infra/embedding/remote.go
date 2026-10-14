package embedding

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/genai"
)

// Remote embedding. Ingestion running in river-service has it make the Gemini
// batchEmbedContents call with its own leased key: ogen builds the request
// and decodes the response here, with the model, task type and dimensions the
// in-process embedder uses, so both produce the same vectors.

// BatchRequest is one planned batchEmbedContents call, relative to the
// Gemini API's base URL.
type BatchRequest struct {
	Path string
	Body []byte
}

type batchBody struct {
	Requests []embedRequest `json:"requests"`
}

type embedRequest struct {
	Model                string       `json:"model"`
	Content              embedContent `json:"content"`
	TaskType             string       `json:"taskType,omitempty"`
	OutputDimensionality *int32       `json:"outputDimensionality,omitempty"`
}

type embedContent struct {
	Parts []embedPart `json:"parts"`
}

type embedPart struct {
	Text string `json:"text"`
}

// apiModel is model as the REST API names it: the bare model id, without the
// Genkit plugin prefix.
func apiModel(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		return model[i+1:]
	}
	return model
}

// PlanBatch builds the request embedding texts with model under cfg.
func PlanBatch(model string, texts []string, cfg *genai.EmbedContentConfig) (BatchRequest, error) {
	if len(texts) == 0 {
		return BatchRequest{}, errors.New("embedding: no texts")
	}
	m := apiModel(model)
	body := batchBody{Requests: make([]embedRequest, len(texts))}
	for i, t := range texts {
		r := embedRequest{Model: "models/" + m, Content: embedContent{Parts: []embedPart{{Text: t}}}}
		if cfg != nil {
			r.TaskType = cfg.TaskType
			r.OutputDimensionality = cfg.OutputDimensionality
		}
		body.Requests[i] = r
	}
	b, err := json.Marshal(body)
	if err != nil {
		return BatchRequest{}, err
	}
	return BatchRequest{Path: "/v1beta/models/" + m + ":batchEmbedContents", Body: b}, nil
}

type batchResponse struct {
	Embeddings []struct {
		Values []float32 `json:"values"`
	} `json:"embeddings"`
}

type apiError struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// DecodeBatch reads the response to a planned batch of n texts: one vector
// per text, index-aligned, or why there are none.
func DecodeBatch(status int, body []byte, n int) ([][]float32, error) {
	if status < 200 || status >= 300 {
		var e apiError
		if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
			return nil, fmt.Errorf("embedding: gemini %d: %s", status, e.Error.Message)
		}
		return nil, fmt.Errorf("embedding: gemini %d", status)
	}
	var resp batchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("embedding: decode response: %w", err)
	}
	if len(resp.Embeddings) != n {
		return nil, fmt.Errorf("embedding: got %d embeddings for %d texts", len(resp.Embeddings), n)
	}
	out := make([][]float32, n)
	for i, e := range resp.Embeddings {
		if len(e.Values) == 0 {
			return nil, fmt.Errorf("embedding: empty embedding %d", i)
		}
		out[i] = e.Values
	}
	return out, nil
}
