package handlers

import (
	"errors"
	"net/url"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/listquery"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// queryValuesOf is the request's query string as url.Values, the form the
// list parsers read. A malformed pair is skipped, as fiber skips it.
func queryValuesOf(c *fiber.Ctx) url.Values {
	v := url.Values{}
	for key, value := range c.Context().QueryArgs().All() {
		v.Add(string(key), string(value))
	}
	return v
}

// listQueryFailure maps a rejected list parameter or cursor to its coded 400;
// any other error passes through.
func listQueryFailure(c *fiber.Ctx, err error) error {
	if qe, ok := errors.AsType[*listquery.Error](err); ok {
		return rejectCode(c, fiber.StatusBadRequest, qe.Code, qe.Msg)
	}
	if errors.Is(err, repository.ErrInvalidCursor) {
		return rejectCode(c, fiber.StatusBadRequest, listquery.CodeInvalidCursor, "cursor is not from this list or this sort")
	}
	return err
}
