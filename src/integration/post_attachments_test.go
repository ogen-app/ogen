//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/transport/grpc/client/pdf"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// LinkedIn Sqid from the platform seed migration. Same constant the
// handler unit tests use; integration tests get the same row through
// the migration chain so it works here too.
const linkedinPlatformID = "AXqWG7U2qnpt"

// makePNG returns a deterministic NxN PNG with a smooth gradient.
// PNG's deflate compresses this very well — fine for tests that only
// need a valid image, useless for size-sensitive ones (use makeNoisyPNG).
func makePNG(side int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	for y := range side {
		for x := range side {
			img.Set(x, y, color.RGBA{R: byte(x), G: byte(y), B: 0, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// makeNoisyPNG returns an NxN PNG filled with seeded pseudo-random
// pixels so deflate cannot compress it down. Used by the streaming
// test, which needs the encoded body to actually be multi-megabyte.
func makeNoisyPNG(side int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	r := rand.New(rand.NewSource(1)) // deterministic across runs
	pix := img.Pix
	r.Read(pix)
	// Force alpha to 0xFF so the PNG encoder picks the non-paletted
	// 32-bit-RGBA path; otherwise it tries to find an optimal encoding
	// that may compress better than we want.
	for i := 3; i < len(pix); i += 4 {
		pix[i] = 0xff
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func multipartBody(field, filename, contentType string, content []byte) (*bytes.Buffer, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	hdr := make(map[string][]string)
	hdr["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name=%q; filename=%q`, field, filename)}
	hdr["Content-Type"] = []string{contentType}
	fw, _ := w.CreatePart(hdr)
	_, _ = fw.Write(content)
	w.Close()
	return &buf, w.FormDataContentType()
}

var _ = Describe("Post attachments — real S3 (MinIO)", Ordered, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		store      storage.Storage
		raw        *s3.Client
		bucket     string
		authCookie *http.Cookie
		campaignID string

		postAttRepo repository.PostAttachmentRepository
	)

	BeforeAll(func() {
		db = mustOpenIntegrationDB()
		store, raw, bucket = mustOpenIntegrationStorage()
	})

	BeforeEach(func() {
		app = fiber.New(fiber.Config{
			BodyLimit: 60 << 20,
			ErrorHandler: func(c *fiber.Ctx, err error) error {
				code := fiber.StatusInternalServerError
				if e, ok := err.(*fiber.Error); ok {
					code = e.Code
				}
				return c.Status(code).JSON(fiber.Map{"error": err.Error()})
			},
		})
		userRepo := repository.NewUserRepository(db)
		sessionRepo := repository.NewSessionRepository(db)
		settingRepo := repository.NewSettingRepository(db)
		tagRepo := repository.NewTagRepository(db)
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo := repository.NewCampaignRepository(db, tagRepo, repository.NewPlatformRepository(db), campaignTypeRepo)
		postRepo := repository.NewPostRepository(db)
		postAttRepo = repository.NewPostAttachmentRepository(db)
		postVersionRepo := repository.NewPostVersionRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, "test_session")

		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, "test_session", false).Register(app)
		handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil).Register(app)

		postsHandler := handlers.NewPostsHandler(postRepo, postVersionRepo, repository.NewPlatformRepository(db), postAttRepo, auth)
		// Wire the same S3-cleanup hook the production server wires —
		// this is exactly what test #3 exercises.
		postsHandler.SetOnBeforeDelete(func(ctx context.Context, postID string) error {
			keys, err := postAttRepo.ListS3KeysByPostID(ctx, postID)
			if err != nil {
				return err
			}
			for _, k := range keys {
				if k == "" {
					continue
				}
				if err := store.Delete(ctx, k); err != nil {
					return err
				}
			}
			return nil
		})
		postsHandler.Register(app)
		handlers.NewPostAttachmentsHandler(postAttRepo, postRepo, store, fakePDFRenderer{}, nil, auth).Register(app)

		seedTenantUser(db, "Admin", "it@example.com", "it-password")

		loginBody, _ := json.Marshal(fiber.Map{"email": "it@example.com", "password": "it-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.StatusCode).To(Equal(fiber.StatusCreated))
		authCookie = loginResp.Cookies()[0]

		cBody, _ := json.Marshal(fiber.Map{"name": "IT Campaign", "campaign_type_id": "Uk"})
		cReq := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(cBody))
		cReq.Header.Set("Content-Type", "application/json")
		cReq.AddCookie(authCookie)
		cResp, err := app.Test(cReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(cResp.StatusCode).To(Equal(fiber.StatusCreated))
		var camp models.Campaign
		Expect(json.NewDecoder(cResp.Body).Decode(&camp)).To(Succeed())
		campaignID = camp.ID
	})

	AfterEach(func() {
		ctx := tenantCtx()
		_, _ = db.NewDelete().TableExpr("post_attachments").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("post_versions").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("post_assistant_messages").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("posts").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("campaigns").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(ctx)
	})

	createPost := func() string {
		body, _ := json.Marshal(fiber.Map{
			"campaign_id":        campaignID,
			"platform_id":        linkedinPlatformID,
			"platform_post_type": "image-post",
			"title":              "IT Post",
		})
		req := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var p models.Post
		Expect(json.NewDecoder(resp.Body).Decode(&p)).To(Succeed())
		return p.ID
	}

	uploadPNG := func(postID string, content []byte) map[string]any {
		body, ct := multipartBody("file", "image.png", "image/png", content)
		req := httptest.NewRequest("POST", "/api/posts/"+postID+"/attachments", body)
		req.Header.Set("Content-Type", ct)
		req.AddCookie(authCookie)
		resp, err := app.Test(req, 60000)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var att map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&att)).To(Succeed())
		return att
	}

	// ── Test #1: round-trip upload via real S3 ──────────────────────────────

	It("#1 round-trip: presigned URL returns the exact bytes we uploaded", func() {
		postID := createPost()
		content := makePNG(8)
		att := uploadPNG(postID, content)

		presigned, ok := att["presigned_url"].(string)
		Expect(ok).To(BeTrue(), "presigned_url should be set")
		Expect(presigned).NotTo(BeEmpty())

		// Cookieless client — we must NOT need our session for the
		// presigned URL to work; the signature is the auth.
		resp, err := http.Get(presigned)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(200))

		gotBytes, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(gotBytes).To(Equal(content))

		gotSum := sha256.Sum256(gotBytes)
		Expect(hex.EncodeToString(gotSum[:])).To(Equal(att["checksum_sha256"]))
	})

	// ── Test #2: DELETE attachment removes the S3 object ───────────────────

	It("#2 DELETE attachment removes the S3 object", func() {
		postID := createPost()
		att := uploadPNG(postID, makePNG(4))
		key := att["s3_key"].(string)
		Expect(objectExists(tenantCtx(), raw, bucket, key)).To(BeTrue())

		req := httptest.NewRequest("DELETE", "/api/posts/"+postID+"/attachments/"+att["id"].(string), nil)
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(204))

		Expect(objectExists(tenantCtx(), raw, bucket, key)).To(BeFalse())
	})

	// ── Test #3: DELETE post cascades attachment objects to S3 ─────────────

	It("#3 DELETE post cleans up every attachment from the bucket", func() {
		postID := createPost()
		a1 := uploadPNG(postID, makePNG(4))
		a2 := uploadPNG(postID, makePNG(5))
		a3 := uploadPNG(postID, makePNG(6))
		keys := []string{a1["s3_key"].(string), a2["s3_key"].(string), a3["s3_key"].(string)}
		for _, k := range keys {
			Expect(objectExists(tenantCtx(), raw, bucket, k)).To(BeTrue())
		}

		req := httptest.NewRequest("DELETE", "/api/posts/"+postID, nil)
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(204))

		for _, k := range keys {
			Expect(objectExists(tenantCtx(), raw, bucket, k)).To(BeFalse(), "object %s should be gone", k)
		}
	})

	// ── Test #4: concurrent uploads to the same post race-free ─────────────

	It("#4 concurrent uploads assign distinct positions without unique-constraint failure", func() {
		postID := createPost()

		const N = 6
		var wg sync.WaitGroup
		results := make(chan map[string]any, N)
		errs := make(chan error, N)
		for i := range N {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				body, ct := multipartBody("file", fmt.Sprintf("img-%d.png", i), "image/png", makePNG(4))
				req := httptest.NewRequest("POST", "/api/posts/"+postID+"/attachments", body)
				req.Header.Set("Content-Type", ct)
				req.AddCookie(authCookie)
				resp, err := app.Test(req, 60000)
				if err != nil {
					errs <- err
					return
				}
				if resp.StatusCode != fiber.StatusCreated {
					bodyBytes, _ := io.ReadAll(resp.Body)
					errs <- fmt.Errorf("upload %d: status %d body=%s", i, resp.StatusCode, string(bodyBytes))
					return
				}
				var att map[string]any
				if err := json.NewDecoder(resp.Body).Decode(&att); err != nil {
					errs <- err
					return
				}
				results <- att
			}(i)
		}
		wg.Wait()
		close(results)
		close(errs)

		for e := range errs {
			Fail(e.Error())
		}

		seenPositions := map[float64]bool{}
		count := 0
		for att := range results {
			pos := att["position"].(float64)
			Expect(seenPositions).NotTo(HaveKey(pos), "position %v assigned twice", pos)
			seenPositions[pos] = true
			count++
		}
		Expect(count).To(Equal(N))
		Expect(seenPositions).To(HaveLen(N))
	})

	// ── Test #5: invalid uploads never reach S3 ────────────────────────────

	It("#5 invalid uploads return a 4xx and never call PutObject", func() {
		postID := createPost()
		before := countObjects(tenantCtx(), raw, bucket)

		// Plain text named .png — image probe rejects it (415).
		body, ct := multipartBody("file", "evil.png", "image/png", []byte("hello world this is plain text padding to defeat sniffer"))
		req := httptest.NewRequest("POST", "/api/posts/"+postID+"/attachments", body)
		req.Header.Set("Content-Type", ct)
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(415))

		// Fake PDF bytes (magic prefix only) — pdf-service rejects it as an
		// invalid PDF, so the upload is refused (400).
		body, ct = multipartBody("file", "evil.pdf", "application/pdf", []byte("%PDF-1.4\nnot really a pdf"))
		req = httptest.NewRequest("POST", "/api/posts/"+postID+"/attachments", body)
		req.Header.Set("Content-Type", ct)
		req.AddCookie(authCookie)
		resp, err = app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(400))

		after := countObjects(tenantCtx(), raw, bucket)
		Expect(after).To(Equal(before), "no new S3 objects should be created when validation rejects upload")
	})

	// ── Test #6: presigned URL TTL is honored ──────────────────────────────

	It("#6 presigned URL stops working after TTL elapses", func() {
		// Override the package-level TTL so the next upload's presigned
		// URL expires within the test's patience window. Defer restore
		// so other specs aren't affected.
		original := handlers.PresignedURLTTL
		handlers.PresignedURLTTL = 2 * time.Second
		defer func() { handlers.PresignedURLTTL = original }()

		postID := createPost()
		att := uploadPNG(postID, makePNG(4))
		presigned := att["presigned_url"].(string)

		// Sanity — works immediately.
		resp, err := http.Get(presigned)
		Expect(err).NotTo(HaveOccurred())
		_ = resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(200))

		time.Sleep(3 * time.Second)

		expiredResp, err := http.Get(presigned)
		Expect(err).NotTo(HaveOccurred())
		defer expiredResp.Body.Close()
		Expect(expiredResp.StatusCode).To(BeNumerically(">=", 400))
		Expect(expiredResp.StatusCode).To(BeNumerically("<", 500))
	})

	// ── Test #7: large upload completes without OOM ────────────────────────

	It("#7 multi-megabyte upload completes and the bytes round-trip", func() {
		// Noisy 1024×1024 RGBA PNG → ~4 MB encoded (deflate can't
		// compress noise). Catches a regression if the upload path
		// stops streaming and starts buffering naively. (CON-73 §5.)
		content := makeNoisyPNG(1024)
		Expect(len(content)).To(BeNumerically(">", 1<<20))

		postID := createPost()
		att := uploadPNG(postID, content)
		Expect(att["size_bytes"]).To(BeEquivalentTo(len(content)))

		// HeadObject confirms S3 actually has the bytes — not just a
		// metadata row claiming so.
		head, err := raw.HeadObject(tenantCtx(), &s3.HeadObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(att["s3_key"].(string)),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(*head.ContentLength).To(BeEquivalentTo(len(content)))
	})

	// ── Test #8: image_constraints JSON column round-trips ─────────────────

	It("#8 platforms.image_constraints JSON round-trips through bun", func() {
		// Insert a custom platform with structured constraints, then
		// scan it back through the model and assert every field
		// survives. Catches Bun/SQLite JSON1 corner cases for empty
		// strings, NULL handling, and nested arrays.
		ctx := tenantCtx()
		p := &models.Platform{
			ID:          "it-platform",
			Name:        "Integration Platform",
			PostTypes:   models.PostTypeMap{"text-post": "Text"},
			Cadence:     "{}",
			Constraints: "",
			ImageConstraints: models.ImageConstraints{
				MaxFileSizeBytes:      4 * 1024 * 1024,
				AllowedFormats:        []string{"jpeg", "png"},
				AnimatedGIFSupported:  false,
				MaxAttachmentsPerPost: 7,
			},
		}
		_, err := db.NewInsert().Model(p).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			_, _ = db.NewDelete().Model((*models.Platform)(nil)).Where("id = ?", p.ID).Exec(ctx)
		}()

		got := &models.Platform{}
		err = db.NewSelect().Model(got).Where("id = ?", p.ID).Scan(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.ImageConstraints.MaxFileSizeBytes).To(BeEquivalentTo(4 * 1024 * 1024))
		Expect(got.ImageConstraints.AllowedFormats).To(ConsistOf("jpeg", "png"))
		Expect(got.ImageConstraints.AnimatedGIFSupported).To(BeFalse())
		Expect(got.ImageConstraints.MaxAttachmentsPerPost).To(Equal(7))
		Expect(got.ImageConstraints.IsZero()).To(BeFalse())
	})

	// ── CON-75: PDF round-trip ─────────────────────────────────────────────

	uploadPDF := func(postID string, content []byte) map[string]any {
		body, ct := multipartBody("file", "doc.pdf", "application/pdf", content)
		req := httptest.NewRequest("POST", "/api/posts/"+postID+"/attachments", body)
		req.Header.Set("Content-Type", ct)
		req.AddCookie(authCookie)
		resp, err := app.Test(req, 60000)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var att map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&att)).To(Succeed())
		return att
	}

	It("#9 round-trip: PDF presigned URL returns the exact bytes we uploaded", func() {
		postID := createPost()
		content := buildIntegrationPDF(2)
		att := uploadPDF(postID, content)

		Expect(att["mime_type"]).To(Equal("application/pdf"))
		Expect(att["page_count"]).To(BeEquivalentTo(2))

		presigned := att["presigned_url"].(string)
		Expect(presigned).NotTo(BeEmpty())

		resp, err := http.Get(presigned)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(200))

		gotBytes, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(gotBytes).To(Equal(content))

		gotSum := sha256.Sum256(gotBytes)
		Expect(hex.EncodeToString(gotSum[:])).To(Equal(att["checksum_sha256"]))
	})

	It("#10 PDF cascade: DELETE post clears both blob and thumbnail keys from the bucket", func() {
		postID := createPost()
		att := uploadPDF(postID, buildIntegrationPDF(1))

		key := att["s3_key"].(string)
		thumbKey, _ := att["thumbnail_s3_key"].(string)
		Expect(objectExists(tenantCtx(), raw, bucket, key)).To(BeTrue())
		if thumbKey != "" {
			Expect(objectExists(tenantCtx(), raw, bucket, thumbKey)).To(BeTrue())
		}

		req := httptest.NewRequest("DELETE", "/api/posts/"+postID, nil)
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(204))

		Expect(objectExists(tenantCtx(), raw, bucket, key)).To(BeFalse())
		if thumbKey != "" {
			Expect(objectExists(tenantCtx(), raw, bucket, thumbKey)).To(BeFalse())
		}
	})

	It("#11 platforms.pdf_constraints JSON round-trips through bun", func() {
		ctx := tenantCtx()
		p := &models.Platform{
			ID:          "it-pdf-platform",
			Name:        "Integration PDF Platform",
			PostTypes:   models.PostTypeMap{"document": "Document"},
			Cadence:     "{}",
			Constraints: "",
			PDFConstraints: models.PDFConstraints{
				MaxFileSizeBytes:      50 * 1024 * 1024,
				AllowedFormats:        []string{"pdf"},
				MaxPages:              42,
				MaxAttachmentsPerPost: 1,
			},
		}
		_, err := db.NewInsert().Model(p).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			_, _ = db.NewDelete().Model((*models.Platform)(nil)).Where("id = ?", p.ID).Exec(ctx)
		}()

		got := &models.Platform{}
		err = db.NewSelect().Model(got).Where("id = ?", p.ID).Scan(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.PDFConstraints.MaxFileSizeBytes).To(BeEquivalentTo(50 * 1024 * 1024))
		Expect(got.PDFConstraints.AllowedFormats).To(ConsistOf("pdf"))
		Expect(got.PDFConstraints.MaxPages).To(Equal(42))
		Expect(got.PDFConstraints.MaxAttachmentsPerPost).To(Equal(1))
		Expect(got.PDFConstraints.IsZero()).To(BeFalse())
	})
})

// buildIntegrationPDF builds a structurally-valid n-page PDF with
// correct xref offsets — same shape as the handler-test fixture, kept
// here so the integration suite stays self-contained.
// fakePDFRenderer stands in for pdf-service in attachment tests: it counts
// pages from the fixture's "/Type /Page /Parent" markers and returns a stub
// thumbnail. With no parseable page it mirrors pdf-service's terminal
// InvalidArgument verdict so the handler rejects magic-bytes-only garbage.
type fakePDFRenderer struct{}

func (fakePDFRenderer) Render(_ context.Context, r io.Reader, _ pdf.RenderOptions) (*pdf.RenderResult, error) {
	data, _ := io.ReadAll(r)
	pages := bytes.Count(data, []byte("/Type /Page /Parent"))
	if pages == 0 {
		return nil, status.Error(codes.InvalidArgument, "fake: not a parseable PDF")
	}
	return &pdf.RenderResult{PageCount: pages, ThumbnailPNG: []byte("PNGTHUMB")}, nil
}

func buildIntegrationPDF(n int) []byte {
	if n < 1 {
		n = 1
	}
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")

	offsets := make([]int, 0, 2+n)
	offsets = append(offsets, buf.Len())
	buf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")

	offsets = append(offsets, buf.Len())
	buf.WriteString("2 0 obj\n<< /Type /Pages /Kids [")
	for i := range n {
		if i > 0 {
			buf.WriteString(" ")
		}
		buf.WriteString(fmt.Sprintf("%d 0 R", 3+i))
	}
	buf.WriteString(fmt.Sprintf("] /Count %d >>\nendobj\n", n))

	for i := range n {
		offsets = append(offsets, buf.Len())
		buf.WriteString(fmt.Sprintf("%d 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>\nendobj\n", 3+i))
	}

	xrefStart := buf.Len()
	totalObjs := 2 + n
	buf.WriteString(fmt.Sprintf("xref\n0 %d\n", totalObjs+1))
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		buf.WriteString(fmt.Sprintf("%010d 00000 n \n", off))
	}

	buf.WriteString(fmt.Sprintf("trailer\n<< /Size %d /Root 1 0 R >>\n", totalObjs+1))
	buf.WriteString(fmt.Sprintf("startxref\n%d\n", xrefStart))
	buf.WriteString("%%EOF\n")
	return buf.Bytes()
}
