package queues_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/jobs/queues"
)

// fakeAttachmentRepo returns a fixed attachment list (CON-122 media tests).
type fakeAttachmentRepo struct{ atts []models.PostAttachment }

func (r *fakeAttachmentRepo) ListByPostID(context.Context, string) ([]models.PostAttachment, error) {
	return r.atts, nil
}
func (r *fakeAttachmentRepo) ListS3KeysByPostID(context.Context, string) ([]string, error) {
	return nil, nil
}
func (r *fakeAttachmentRepo) GetByID(context.Context, string) (*models.PostAttachment, error) {
	return nil, nil
}
func (r *fakeAttachmentRepo) CreateAtNextPosition(context.Context, *models.PostAttachment) error {
	return nil
}
func (r *fakeAttachmentRepo) UpdatePosition(context.Context, string, int) error   { return nil }
func (r *fakeAttachmentRepo) UpdateAltText(context.Context, string, string) error { return nil }
func (r *fakeAttachmentRepo) SetGeneratedAltText(context.Context, string, string) error {
	return nil
}
func (r *fakeAttachmentRepo) UpdateSegmentIndex(context.Context, string, *int) error   { return nil }
func (r *fakeAttachmentRepo) ReorderPositions(context.Context, string, []string) error { return nil }
func (r *fakeAttachmentRepo) Delete(context.Context, string) (bool, error)             { return false, nil }

// fakeStorage serves attachment bytes by key; other methods are no-ops.
type fakeStorage struct{ objects map[string][]byte }

func (s *fakeStorage) Upload(context.Context, string, io.Reader, int64, string) (string, error) {
	return "", nil
}
func (s *fakeStorage) Copy(context.Context, string, string) error { return nil }
func (s *fakeStorage) Delete(context.Context, string) error       { return nil }
func (s *fakeStorage) PublicURL(string) string                    { return "" }
func (s *fakeStorage) PresignedGetURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}
func (s *fakeStorage) PresignedPutURL(context.Context, string, string, time.Duration) (string, error) {
	return "", nil
}
func (s *fakeStorage) Head(context.Context, string) (*storage.ObjectInfo, error) {
	return &storage.ObjectInfo{}, nil
}
func (s *fakeStorage) Download(_ context.Context, key string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.objects[key])), nil
}

// TestSubmitUploadsMediaWithAltText proves the CON-122 media flow end-to-end:
// the worker uploads each attachment to Zernio (presign → PUT bytes) and the
// create-post body references the returned publicUrl with type + altText.
func TestSubmitUploadsMediaWithAltText(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()

	var uploaded []byte
	var uploadedCT string
	stub.handle("POST", "/media/presign", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["filename"] == "" || body["contentType"] == "" {
			t.Errorf("presign missing filename/contentType: %v", body)
		}
		writeJSON(w, http.StatusOK, zernio.MediaPresign{
			UploadURL: stub.URL + "/up/one",
			PublicURL: "https://cdn.zernio.test/one.png",
			Key:       "k1",
			ExpiresIn: 3600,
		})
	})
	stub.handle("PUT", "/up/one", func(w http.ResponseWriter, r *http.Request) {
		uploaded, _ = io.ReadAll(r.Body)
		uploadedCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	})
	var submitBody zernio.SubmitRequest
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&submitBody)
		writeJSON(w, http.StatusCreated, zernio.PostEnvelope{Post: zernio.Job{
			ID: "z-1", Status: zernio.JobStatusScheduled,
		}})
	})

	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-1", Platform: "linkedin"}},
	})
	deps.Storage = &fakeStorage{objects: map[string][]byte{
		"post-attachments/post-1/att1.png": []byte("PNGBYTES"),
	}}
	deps.PostAttachmentRepo = &fakeAttachmentRepo{atts: []models.PostAttachment{{
		ID: "att1", PostID: "post-1", Position: 0,
		MimeType: "image/png", SizeBytes: 8,
		S3Key: "post-attachments/post-1/att1.png", AltText: "a red bicycle",
	}}}
	post := seedScheduledPost(postRepo)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process: %v", err)
	}

	if string(uploaded) != "PNGBYTES" {
		t.Errorf("uploaded bytes = %q, want PNGBYTES", uploaded)
	}
	if uploadedCT != "image/png" {
		t.Errorf("upload content-type = %q, want image/png", uploadedCT)
	}
	if len(submitBody.MediaItems) != 1 {
		t.Fatalf("mediaItems = %d, want 1", len(submitBody.MediaItems))
	}
	m := submitBody.MediaItems[0]
	if m["url"] != "https://cdn.zernio.test/one.png" || m["type"] != "image" || m["altText"] != "a red bicycle" {
		t.Errorf("mediaItem wrong: %+v", m)
	}
}

// TestSubmitThreadBuildsThreadItems proves the CON-284 egress: a thread post is
// submitted with per-segment platformSpecificData.threadItems (each carrying its
// own content + media), the top-level content mirrors the root, and top-level
// mediaItems stays empty (media rides inside the segments).
func TestSubmitThreadBuildsThreadItems(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()

	// Presign echoes the requested filename into the public URL so we can tell
	// which uploaded object landed on which segment.
	stub.handle("POST", "/media/presign", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, http.StatusOK, zernio.MediaPresign{
			UploadURL: stub.URL + "/up",
			PublicURL: "https://cdn.zernio.test/" + body["filename"],
			Key:       body["filename"],
			ExpiresIn: 3600,
		})
	})
	stub.handle("PUT", "/up", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	var submitBody zernio.SubmitRequest
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&submitBody)
		writeJSON(w, http.StatusCreated, zernio.PostEnvelope{Post: zernio.Job{
			ID: "z-thr", Status: zernio.JobStatusScheduled,
		}})
	})

	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-x", Platform: "twitter"}},
	})
	deps.Storage = &fakeStorage{objects: map[string][]byte{
		"post-attachments/t-1/a.png": []byte("A"),
		"post-attachments/t-1/b.jpg": []byte("B"),
	}}
	// Media on segment 0 (a.png) and segment 1 (b.jpg).
	seg0, seg1 := 0, 1
	deps.PostAttachmentRepo = &fakeAttachmentRepo{atts: []models.PostAttachment{
		{ID: "att-a", PostID: "t-1", Position: 0, SegmentIndex: &seg0, MimeType: "image/png", SizeBytes: 1, S3Key: "post-attachments/t-1/a.png", AltText: "alpha"},
		{ID: "att-b", PostID: "t-1", Position: 1, SegmentIndex: &seg1, MimeType: "image/jpeg", SizeBytes: 1, S3Key: "post-attachments/t-1/b.jpg"},
	}}

	now := time.Now().Add(-time.Minute).UTC()
	post := &models.Post{
		ID:               "t-1",
		PlatformID:       "81mUCmc2xsKd", // X (Twitter) Sqid → zernioID "twitter"
		PlatformPostType: models.PostTypeThread,
		Content:          "root msg\n\n---\n\nreply msg", // R2: content is the full thread body
		ThreadSegments:   models.ThreadSegments{{Content: "root msg"}, {Content: "reply msg"}},
		Status:           models.PostStatusScheduled,
		ScheduledAt:      &now,
		Platform:         &models.Platform{ID: "81mUCmc2xsKd", Name: "X (Twitter)"},
	}
	postRepo.put(post)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process: %v", err)
	}

	// Top-level content mirrors the root; top-level media is empty for a thread.
	if submitBody.Content != "root msg" {
		t.Errorf("top-level content = %q, want root mirror %q", submitBody.Content, "root msg")
	}
	if len(submitBody.MediaItems) != 0 {
		t.Errorf("top-level mediaItems = %d, want 0 (media rides in threadItems)", len(submitBody.MediaItems))
	}

	if len(submitBody.Platforms) != 1 || submitBody.Platforms[0].PlatformSpecificData == nil {
		t.Fatalf("expected one platform variant carrying platformSpecificData, got %+v", submitBody.Platforms)
	}
	items := submitBody.Platforms[0].PlatformSpecificData.ThreadItems
	if len(items) != 2 {
		t.Fatalf("threadItems = %d, want 2", len(items))
	}
	if items[0].Content != "root msg" || items[1].Content != "reply msg" {
		t.Errorf("threadItem contents wrong: %q / %q", items[0].Content, items[1].Content)
	}
	// Segment 0 carries a.png (with its altText); segment 1 carries b.jpg.
	if len(items[0].MediaItems) != 1 || items[0].MediaItems[0]["url"] != "https://cdn.zernio.test/a.png" || items[0].MediaItems[0]["altText"] != "alpha" {
		t.Errorf("segment 0 media wrong: %+v", items[0].MediaItems)
	}
	if len(items[1].MediaItems) != 1 || items[1].MediaItems[0]["url"] != "https://cdn.zernio.test/b.jpg" {
		t.Errorf("segment 1 media wrong: %+v", items[1].MediaItems)
	}
}

// TestSubmitThreadNilIndexMediaOnRoot proves CON-284 R2: an attachment with a
// NULL segment_index (the delimited-body flow's default) is published on the root
// message (threadItems[0]), not skipped.
func TestSubmitThreadNilIndexMediaOnRoot(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()

	stub.handle("POST", "/media/presign", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, http.StatusOK, zernio.MediaPresign{
			UploadURL: stub.URL + "/up",
			PublicURL: "https://cdn.zernio.test/" + body["filename"],
			Key:       body["filename"],
			ExpiresIn: 3600,
		})
	})
	stub.handle("PUT", "/up", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	var submitBody zernio.SubmitRequest
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&submitBody)
		writeJSON(w, http.StatusCreated, zernio.PostEnvelope{Post: zernio.Job{ID: "z-thr2", Status: zernio.JobStatusScheduled}})
	})

	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-x", Platform: "twitter"}},
	})
	deps.Storage = &fakeStorage{objects: map[string][]byte{"post-attachments/t-2/a.png": []byte("A")}}
	// No SegmentIndex set → nil → must land on the root.
	deps.PostAttachmentRepo = &fakeAttachmentRepo{atts: []models.PostAttachment{
		{ID: "att-a", PostID: "t-2", Position: 0, MimeType: "image/png", SizeBytes: 1, S3Key: "post-attachments/t-2/a.png"},
	}}

	now := time.Now().Add(-time.Minute).UTC()
	post := &models.Post{
		ID:               "t-2",
		PlatformID:       "81mUCmc2xsKd",
		PlatformPostType: models.PostTypeThread,
		Content:          "root msg\n\n---\n\nreply msg",
		ThreadSegments:   models.ThreadSegments{{Content: "root msg"}, {Content: "reply msg"}},
		Status:           models.PostStatusScheduled,
		ScheduledAt:      &now,
		Platform:         &models.Platform{ID: "81mUCmc2xsKd", Name: "X (Twitter)"},
	}
	postRepo.put(post)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process: %v", err)
	}

	items := submitBody.Platforms[0].PlatformSpecificData.ThreadItems
	if len(items) != 2 {
		t.Fatalf("threadItems = %d, want 2", len(items))
	}
	if len(items[0].MediaItems) != 1 || items[0].MediaItems[0]["url"] != "https://cdn.zernio.test/a.png" {
		t.Errorf("nil-index media: want on root (segment 0), got root=%+v reply=%+v", items[0].MediaItems, items[1].MediaItems)
	}
	if len(items[1].MediaItems) != 0 {
		t.Errorf("reply segment should have no media, got %+v", items[1].MediaItems)
	}
}
