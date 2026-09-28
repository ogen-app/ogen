package handlers_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// stubHub satisfies eventhub.Hub for handler tests. Events pushed by the
// test land on a channel handed back to the handler's Subscribe call;
// closing the channel from the test side ends the SSE stream.
type stubHub struct {
	ch         chan eventhub.Event
	subscribed atomic.Int32
	unsubbed   atomic.Int32
	subErr     error
}

func newStubHub(buffer int) *stubHub {
	return &stubHub{ch: make(chan eventhub.Event, buffer)}
}

func (s *stubHub) Subscribe(_ context.Context, _ eventhub.SubscribeOpts) (<-chan eventhub.Event, func(), error) {
	if s.subErr != nil {
		return nil, nil, s.subErr
	}
	s.subscribed.Add(1)
	return s.ch, func() { s.unsubbed.Add(1) }, nil
}

func (s *stubHub) Publish(_ context.Context, ev eventhub.Event) error {
	s.ch <- ev
	return nil
}

type sseFrame struct {
	id    string
	event string
	data  string
}

// parseSSEStream reads the response body until EOF and groups the lines
// into frames separated by blank lines. SSE comment lines (starting with
// `:`) become frames with event="" id="" data="" so the test can assert
// the heartbeat fired.
func parseSSEStream(r io.Reader) []sseFrame {
	frames := []sseFrame{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var cur sseFrame
	var hasContent bool
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
			hasContent = true
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
			hasContent = true
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
			hasContent = true
		case strings.HasPrefix(line, ": "):
			// Heartbeat comment frame.
			frames = append(frames, sseFrame{})
			hasContent = false
		case line == "":
			if hasContent {
				frames = append(frames, cur)
				cur = sseFrame{}
				hasContent = false
			}
		}
	}
	return frames
}

var _ = Describe("EventsHandler", Ordered, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		authCookie *http.Cookie
		hub        *stubHub
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
	})

	BeforeEach(func() {
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
		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth, nil, nil).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false, nil).Register(app)

		hub = newStubHub(8)
		// Big heartbeat so tests don't see secondary heartbeats. The
		// initial heartbeat (sent right after Subscribe) IS asserted on
		// in the streaming spec.
		handlers.NewEventsHandler(hub, sessionRepo, auth, time.Hour).Register(app)

		seedTenantUser(db, "Admin", "admin@example.com", "admin-password")

		loginBody, _ := json.Marshal(fiber.Map{"email": "admin@example.com", "password": "admin-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq, 5000)
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
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("GET /api/events", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/events?topics=all", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns 400 when topics is missing", func() {
				req := httptest.NewRequest("GET", "/api/events", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("returns 400 when topics is empty", func() {
				req := httptest.NewRequest("GET", "/api/events?topics=", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("returns 400 when topics has only commas/whitespace", func() {
				req := httptest.NewRequest("GET", "/api/events?topics=,%20,", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("streams events as SSE frames and ends when the hub channel closes", func() {
				// Pre-populate before the request so the handler emits
				// them in order, then we close to terminate.
				hub.ch <- eventhub.Event{ID: "ev-1", Topic: "job:foo", Type: "completed", Payload: map[string]string{"k": "v"}}
				hub.ch <- eventhub.Event{ID: "ev-2", Topic: "entity:post:abc", Type: "updated"}
				close(hub.ch)

				req := httptest.NewRequest("GET", "/api/events?topics=all", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req, 5000)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))
				Expect(resp.Header.Get("Content-Type")).To(ContainSubstring("text/event-stream"))
				Expect(resp.Header.Get("Cache-Control")).To(ContainSubstring("no-cache"))
				Expect(resp.Header.Get("X-Accel-Buffering")).To(Equal("no"))

				frames := parseSSEStream(resp.Body)
				// One initial heartbeat (empty frame) + two events.
				Expect(len(frames)).To(BeNumerically(">=", 3))
				Expect(frames[0]).To(Equal(sseFrame{}), "first frame should be the initial heartbeat")
				Expect(frames[1].id).To(Equal("ev-1"))
				Expect(frames[1].event).To(Equal("completed"))
				Expect(frames[1].data).To(ContainSubstring(`"topic":"job:foo"`))
				Expect(frames[2].id).To(Equal("ev-2"))
				Expect(frames[2].event).To(Equal("updated"))
				Expect(frames[2].data).To(ContainSubstring(`"topic":"entity:post:abc"`))

				// Subscribe was called once, and the handler released
				// the subscription on the way out.
				Expect(hub.subscribed.Load()).To(Equal(int32(1)))
				Expect(hub.unsubbed.Load()).To(Equal(int32(1)))
			})

			It("falls back to event=message when Type is empty", func() {
				hub.ch <- eventhub.Event{ID: "ev-untyped", Topic: "all"}
				close(hub.ch)

				req := httptest.NewRequest("GET", "/api/events?topics=all", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req, 5000)
				Expect(err).NotTo(HaveOccurred())

				frames := parseSSEStream(resp.Body)
				// frames[0] = heartbeat, frames[1] = the event
				Expect(len(frames)).To(BeNumerically(">=", 2))
				Expect(frames[1].event).To(Equal("message"))
			})

			It("returns 429 when the hub reports the user has too many subscribers", func() {
				hub.subErr = eventhub.ErrTooManySubscribers
				req := httptest.NewRequest("GET", "/api/events?topics=all", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req, 5000)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(429))
			})
		})
	})
})
