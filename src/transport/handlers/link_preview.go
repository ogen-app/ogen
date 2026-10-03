package handlers

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/usecase/linkpreview"
)

// linkPreviewsPerMinute bounds outbound page fetches per workspace. The
// composer debounces, and cached URLs don't count against a fetch, but each
// call is still a request to an arbitrary site made on the tenant's behalf.
const linkPreviewsPerMinute = 60

// LinkPreviewer resolves a URL to its preview card data.
type LinkPreviewer interface {
	Get(ctx context.Context, rawURL string) (linkpreview.Preview, error)
}

type LinkPreviewHandler struct {
	previews LinkPreviewer
	auth     fiber.Handler
	limits   *tenantWindowLimiter
}

func NewLinkPreviewHandler(previews LinkPreviewer, auth fiber.Handler) *LinkPreviewHandler {
	return &LinkPreviewHandler{
		previews: previews,
		auth:     auth,
		limits:   newTenantWindowLimiter(linkPreviewsPerMinute, time.Minute),
	}
}

func (h *LinkPreviewHandler) Register(app *fiber.App) {
	app.Get("/api/link-preview", h.auth, h.Get)
}

// Get godoc
// @Summary      Preview a link
// @Description  Fetches the page's Open Graph tags so the composer can render the
// @Description  card a network builds for a link post. Approximate: the network
// @Description  reads the page itself at publish time. A page that can't be read
// @Description  still returns 200 with only `url` and `domain` set.
// @Tags         posts
// @Produce      json
// @Security     CookieAuth
// @Param        url  query     string  true  "Absolute http(s) URL"
// @Success      200  {object}  linkpreview.Preview
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      429  {object}  map[string]string
// @Router       /api/link-preview [get]
func (h *LinkPreviewHandler) Get(c *fiber.Ctx) error {
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	if !h.limits.allow(session.TenantID) {
		return fiber.NewError(fiber.StatusTooManyRequests, "too many link previews; try again shortly")
	}
	p, err := h.previews.Get(reqCtx(c), c.Query("url"))
	if errors.Is(err, linkpreview.ErrInvalidURL) {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if err != nil {
		return err
	}
	return c.JSON(p)
}

// tenantWindowLimiter is a fixed-window counter per tenant, in memory and per
// instance.
type tenantWindowLimiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	windows map[string]tenantWindow
}

type tenantWindow struct {
	start time.Time
	count int
}

func newTenantWindowLimiter(limit int, window time.Duration) *tenantWindowLimiter {
	return &tenantWindowLimiter{limit: limit, window: window, now: time.Now, windows: map[string]tenantWindow{}}
}

func (l *tenantWindowLimiter) allow(tenantID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.windows[tenantID]
	if now.Sub(w.start) >= l.window {
		for id, old := range l.windows {
			if now.Sub(old.start) >= l.window {
				delete(l.windows, id)
			}
		}
		w = tenantWindow{start: now}
	}
	if w.count >= l.limit {
		return false
	}
	w.count++
	l.windows[tenantID] = w
	return true
}
