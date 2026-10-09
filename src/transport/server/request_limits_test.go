package server

import (
	"bytes"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"

	"github.com/ogen-app/ogen/src/kernel/config"
)

func TestRequestLimits(t *testing.T) {
	app := newFiberApp(&config.Config{})
	app.Post("/echo", func(c *fiber.Ctx) error {
		return c.SendString(c.Get(fiber.HeaderContentType))
	})

	big := bytes.Repeat([]byte("a"), maxBodyBytes+1)

	t.Run("non-multipart body over the cap is refused", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/echo", bytes.NewReader(big))
		req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
		// fasthttp answers 413 and drops the connection; app.Test surfaces the
		// dropped connection as ErrBodyTooLarge rather than the response.
		resp, err := app.Test(req, -1)
		switch {
		case errors.Is(err, fasthttp.ErrBodyTooLarge):
		case err != nil:
			t.Fatalf("app.Test: %v", err)
		case resp.StatusCode != fiber.StatusRequestEntityTooLarge:
			t.Fatalf("status = %d, want 413", resp.StatusCode)
		}
	})

	t.Run("multipart upload of the same size is accepted", func(t *testing.T) {
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		part, err := w.CreateFormFile("file", "big.md")
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := part.Write(big); err != nil {
			t.Fatalf("write form file: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("close multipart writer: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/echo", &body)
		req.Header.Set(fiber.HeaderContentType, w.FormDataContentType())
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})
}
