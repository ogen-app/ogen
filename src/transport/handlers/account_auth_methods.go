package handlers

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/usecase/oauthlogin"
)

// AccountAuthMethodsHandler serves the Settings view of how the signed-in
// account can sign in, and disconnecting a provider. Connecting one goes
// through the provider flow (GET /api/auth/google/start?intent=link); setting a
// first password goes through the password-reset email.
type AccountAuthMethodsHandler struct {
	methods  *oauthlogin.Methods
	accounts repository.AccountRepository
	auth     fiber.Handler
	activity *activity.Recorder
}

// NewAccountAuthMethodsHandler returns the handler.
func NewAccountAuthMethodsHandler(methods *oauthlogin.Methods, accounts repository.AccountRepository, auth fiber.Handler, rec *activity.Recorder) *AccountAuthMethodsHandler {
	return &AccountAuthMethodsHandler{methods: methods, accounts: accounts, auth: auth, activity: rec}
}

func (h *AccountAuthMethodsHandler) Register(app *fiber.App) {
	g := app.Group("/api/account", h.auth)
	g.Get("/auth-methods", h.List)
	g.Delete("/identities/:provider", h.Unlink)
}

// List godoc
// @Summary      Sign-in methods
// @Description  How the signed-in account can sign in: whether it has a password, and its connected providers. An account without a password sets one through POST /api/password-reset.
// @Tags         account
// @Produce      json
// @Security     CookieAuth
// @Success      200  {object}  oauthlogin.AuthMethods
// @Failure      401  {object}  map[string]string
// @Router       /api/account/auth-methods [get]
func (h *AccountAuthMethodsHandler) List(c *fiber.Ctx) error {
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	account, err := h.accounts.GetByID(reqCtx(c), session.AccountID)
	if err != nil {
		return err
	}
	out, err := h.methods.List(reqCtx(c), account)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// Unlink godoc
// @Summary      Disconnect a sign-in provider
// @Description  Disconnects the provider from the signed-in account and signs its other sessions out. Refused with 409 last_sign_in_method when the account has no password, since it could no longer sign in.
// @Tags         account
// @Security     CookieAuth
// @Param        provider  path  string  true  "Provider, e.g. google"
// @Success      204
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Router       /api/account/identities/{provider} [delete]
func (h *AccountAuthMethodsHandler) Unlink(c *fiber.Ctx) error {
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	// Cloned: the activity recorder keeps it past the handler, and fiber's
	// params alias fasthttp buffers that are reused.
	provider := strings.Clone(c.Params("provider"))
	err = h.methods.Unlink(reqCtx(c), session.AccountID, provider, session.ID)
	switch {
	case errors.Is(err, oauthlogin.ErrUnknownProvider), errors.Is(err, oauthlogin.ErrNotLinked):
		return fiber.NewError(fiber.StatusNotFound, "not connected")
	case errors.Is(err, oauthlogin.ErrLastSignInMethod):
		return rejectCode(c, fiber.StatusConflict, "last_sign_in_method", "set a password before disconnecting your only way to sign in")
	case err != nil:
		return err
	}
	h.activity.Record(reqCtx(c), activity.CategoryAuthentication, "identity_unlinked",
		activity.WithEntity("user", session.UserID), activity.WithSource(activity.SourceAPI),
		activity.WithPayload(map[string]any{"provider": provider}))
	return c.SendStatus(fiber.StatusNoContent)
}
