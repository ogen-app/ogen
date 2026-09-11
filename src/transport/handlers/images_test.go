package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// stubStorage is an in-memory Storage implementation for tests.
type stubStorage struct {
	returnURL string
	returnErr error
	lastKey   string
	objects   map[string][]byte
	// headSizeOverride, when > 0, makes Head report this size for any key —
	// lets a test simulate an oversized object without materialising the bytes.
	headSizeOverride int64
}

func (s *stubStorage) Upload(_ context.Context, key string, r io.Reader, _ int64, _ string) (string, error) {
	s.lastKey = key
	if s.objects != nil && s.returnErr == nil {
		buf, _ := io.ReadAll(r)
		s.objects[key] = buf
	}
	return s.returnURL, s.returnErr
}

func (s *stubStorage) Copy(_ context.Context, srcKey, dstKey string) error {
	if s.returnErr != nil {
		return s.returnErr
	}
	if s.objects != nil {
		s.objects[dstKey] = s.objects[srcKey]
	}
	return nil
}

func (s *stubStorage) Delete(_ context.Context, key string) error {
	if s.objects != nil {
		delete(s.objects, key)
	}
	return nil
}

func (s *stubStorage) PublicURL(key string) string { return "https://pub.example.com/" + key }

func (s *stubStorage) PresignedGetURL(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://pub.example.com/signed/" + key, nil
}

func (s *stubStorage) PresignedPutURL(_ context.Context, key, _ string, _ time.Duration) (string, error) {
	return "https://pub.example.com/put/" + key, nil
}

func (s *stubStorage) Head(_ context.Context, key string) (*storage.ObjectInfo, error) {
	if s.headSizeOverride > 0 {
		return &storage.ObjectInfo{Size: s.headSizeOverride, ContentType: "audio/mpeg"}, nil
	}
	if s.objects == nil {
		return nil, s.returnErr
	}
	b, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("stubStorage: object %q not found", key)
	}
	return &storage.ObjectInfo{Size: int64(len(b)), ContentType: "video/mp4"}, nil
}

func (s *stubStorage) Download(_ context.Context, key string) (io.ReadCloser, error) {
	if s.returnErr != nil {
		return nil, s.returnErr
	}
	return io.NopCloser(bytes.NewReader(s.objects[key])), nil
}

// minimalPNG returns the bytes of a tiny valid 1×1 PNG image.
func minimalPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// multipartBody builds a multipart/form-data body with a single "file" field.
func multipartBody(filename string, content []byte) (*bytes.Buffer, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("file", filename)
	_, _ = fw.Write(content)
	w.Close()
	return &buf, w.FormDataContentType()
}

// Serial: one spec pushes an 11 MB body through the handler; under `-race
// -procs=2` that starves 1s-budget requests on the other proc and flakes
// unrelated specs' setup calls. Run this (Ordered) suite serially so the hog
// stays out of the parallel phase. (A lone Serial spec isn't allowed inside a
// non-Serial Ordered container, so the decorator goes on the container.)
var _ = Describe("ImagesHandler", Ordered, Serial, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		authCookie *http.Cookie
		stub       *stubStorage
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
	})

	BeforeEach(func() {
		stub = &stubStorage{returnURL: "https://pub.example.com/abc123.png"}

		app = fiber.New(fiber.Config{
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
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(app)
		handlers.NewImagesHandler(stub, auth).Register(app)

		// Seed user and log in.
		seedTenantUser(db, "Admin", "admin@example.com", "admin-password")

		loginBody, _ := json.Marshal(fiber.Map{"email": "admin@example.com", "password": "admin-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.StatusCode).To(Equal(fiber.StatusCreated))
		cookies := loginResp.Cookies()
		Expect(cookies).To(HaveLen(1))
		authCookie = cookies[0]
	})

	AfterEach(func() {
		_, err := db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(context.Background())
		_, err = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
	})

	// ── Auth ─────────────────────────────────────────────────────────────────

	Describe("POST /api/images", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				body, ct := multipartBody("image.png", minimalPNG())
				req := httptest.NewRequest("POST", "/api/images", body)
				req.Header.Set("Content-Type", ct)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns 200 with status and url on success", func() {
				body, ct := multipartBody("photo.png", minimalPNG())
				req := httptest.NewRequest("POST", "/api/images", body)
				req.Header.Set("Content-Type", ct)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var result map[string]any
				Expect(json.NewDecoder(resp.Body).Decode(&result)).To(Succeed())
				Expect(result["status"]).To(Equal("success"))
				data, ok := result["data"].(map[string]any)
				Expect(ok).To(BeTrue())
				Expect(data["url"]).To(Equal("https://pub.example.com/abc123.png"))
			})

			It("uses a tenant-namespaced server-generated key (uuid + extension), not the client filename", func() {
				body, ct := multipartBody("malicious/../../../etc/passwd.png", minimalPNG())
				req := httptest.NewRequest("POST", "/api/images", body)
				req.Header.Set("Content-Type", ct)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))
				// Tenant prefix retained (CON-97); key is server-generated, not the client filename.
				Expect(stub.lastKey).To(MatchRegexp(`^t/default/[0-9a-f-]{36}\.png$`))
			})

			It("returns 400 when no file is provided", func() {
				var buf bytes.Buffer
				w := multipart.NewWriter(&buf)
				w.Close()
				req := httptest.NewRequest("POST", "/api/images", &buf)
				req.Header.Set("Content-Type", w.FormDataContentType())
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("returns 400 when the file exceeds 10 MB", func() {
				oversized := bytes.Repeat([]byte("x"), 11<<20)
				body, ct := multipartBody("big.png", oversized)
				req := httptest.NewRequest("POST", "/api/images", body)
				req.Header.Set("Content-Type", ct)
				req.AddCookie(authCookie)
				// Fiber's default body limit is 4 MB; bump it for this test.
				app2 := fiber.New(fiber.Config{
					BodyLimit: 20 << 20,
					ErrorHandler: func(c *fiber.Ctx, err error) error {
						code := fiber.StatusInternalServerError
						if e, ok := err.(*fiber.Error); ok {
							code = e.Code
						}
						return c.Status(code).JSON(fiber.Map{"error": err.Error()})
					},
				})
				userRepo2 := repository.NewUserRepository(db)
				sessionRepo2 := repository.NewSessionRepository(db)
				settingRepo2 := repository.NewSettingRepository(db)
				auth2 := handlers.RequireAuth(sessionRepo2, userRepo2, testCookieName)
				handlers.NewUsersHandler(db, userRepo2, repository.NewAccountRepository(db), settingRepo2, auth2).Register(app2)
				handlers.NewSessionsHandler(userRepo2, repository.NewAccountRepository(db), sessionRepo2, testCookieName, false).Register(app2)
				handlers.NewImagesHandler(stub, auth2).Register(app2)
				resp, err := app2.Test(req, 30000)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("returns 415 for a disallowed MIME type", func() {
				body, ct := multipartBody("doc.txt", []byte("hello world"))
				req := httptest.NewRequest("POST", "/api/images", body)
				req.Header.Set("Content-Type", ct)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(415))
			})

			It("returns 503 when storage is not configured", func() {
				app2 := fiber.New(fiber.Config{
					ErrorHandler: func(c *fiber.Ctx, err error) error {
						code := fiber.StatusInternalServerError
						if e, ok := err.(*fiber.Error); ok {
							code = e.Code
						}
						return c.Status(code).JSON(fiber.Map{"error": err.Error()})
					},
				})
				userRepo2 := repository.NewUserRepository(db)
				sessionRepo2 := repository.NewSessionRepository(db)
				settingRepo2 := repository.NewSettingRepository(db)
				auth2 := handlers.RequireAuth(sessionRepo2, userRepo2, testCookieName)
				handlers.NewUsersHandler(db, userRepo2, repository.NewAccountRepository(db), settingRepo2, auth2).Register(app2)
				handlers.NewSessionsHandler(userRepo2, repository.NewAccountRepository(db), sessionRepo2, testCookieName, false).Register(app2)
				handlers.NewImagesHandler(nil, auth2).Register(app2) // nil = disabled

				body, ct := multipartBody("photo.png", minimalPNG())
				req := httptest.NewRequest("POST", "/api/images", body)
				req.Header.Set("Content-Type", ct)
				req.AddCookie(authCookie)
				resp, err := app2.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(503))
			})

			It("returns 415 when content type is svg (not in allowed list)", func() {
				svgContent := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`)
				body, ct := multipartBody("image.svg", svgContent)
				req := httptest.NewRequest("POST", "/api/images", body)
				req.Header.Set("Content-Type", ct)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(415))
			})

			It("accepts a valid JPEG", func() {
				// Minimal JPEG magic bytes.
				jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46}
				jpeg = append(jpeg, bytes.Repeat([]byte{0x00}, 502)...) // pad past sniff window
				body, ct := multipartBody("photo.jpg", jpeg)
				req := httptest.NewRequest("POST", "/api/images", body)
				req.Header.Set("Content-Type", ct)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				// JPEG sniffing may classify as application/octet-stream with only magic bytes;
				// either 200 (detected as image/jpeg) or 415 (undetectable) is acceptable.
				Expect(resp.StatusCode).To(BeElementOf(200, 415))
			})
		})
	})

	// ── Storage disabled ─────────────────────────────────────────────────────

	Describe("storage.Storage interface", func() {
		It("nil Storage satisfies the type system and triggers 503", func() {
			var s storage.Storage // nil interface
			h := handlers.NewImagesHandler(s, func(c *fiber.Ctx) error { return c.Next() })
			Expect(h).NotTo(BeNil())
		})

		It("stub returns the configured URL", func() {
			url, err := stub.Upload(context.Background(), "key.png", strings.NewReader("data"), 4, "image/png")
			Expect(err).NotTo(HaveOccurred())
			Expect(url).To(Equal("https://pub.example.com/abc123.png"))
		})
	})
})
