package handlers

import (
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/ogen-app/ogen/src/infra/storage"
)

const (
	maxImageSize = 10 << 20 // 10 MB
)

var allowedImageMIMEs = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

type ImagesHandler struct {
	storage storage.Storage
	auth    fiber.Handler
}

func NewImagesHandler(store storage.Storage, auth fiber.Handler) *ImagesHandler {
	return &ImagesHandler{storage: store, auth: auth}
}

func (h *ImagesHandler) Register(app *fiber.App) {
	app.Post("/api/images", h.auth, h.Upload)
}

// Upload godoc
// @Summary      Upload image
// @Description  Uploads an image file to object storage and returns its public URL.
//
//	Accepted types: image/jpeg, image/png, image/webp, image/gif. Max size: 10 MB.
//
// @Tags         images
// @Accept       multipart/form-data
// @Produce      json
// @Security     CookieAuth
// @Param        file  formData  file  true  "Image file"
// @Success      200   {object}  map[string]any
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      415   {object}  map[string]string
// @Failure      503   {object}  map[string]string
// @Router       /api/images [post]
func (h *ImagesHandler) Upload(c *fiber.Ctx) error {
	if h.storage == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "storage not configured")
	}

	fh, err := c.FormFile("file")
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "file is required")
	}

	if fh.Size > maxImageSize {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("file exceeds maximum size of %d MB", maxImageSize>>20))
	}

	f, err := fh.Open()
	if err != nil {
		return fmt.Errorf("images: open upload: %w", err)
	}
	defer f.Close()

	// Detect MIME from the first 512 bytes — ignores the client-supplied Content-Type.
	sniff := make([]byte, 512)
	n, err := f.Read(sniff)
	if err != nil {
		return fmt.Errorf("images: read upload: %w", err)
	}
	mimeType := http.DetectContentType(sniff[:n])

	ext, ok := allowedImageMIMEs[mimeType]
	if !ok {
		return fiber.NewError(fiber.StatusUnsupportedMediaType,
			fmt.Sprintf("unsupported media type: %s", mimeType))
	}

	// Rewind so the full file body is uploaded.
	if _, err := f.Seek(0, 0); err != nil {
		return fmt.Errorf("images: seek upload: %w", err)
	}

	// Strip any directory components from the filename for safety, then
	// namespace the object key by tenant (CON-97). filepath.Base must run on
	// the filename — running it on the final key would strip the
	// t/<tenant_id>/ prefix.
	key := storage.TenantKey(reqCtx(c), filepath.Base(uuid.NewString()+ext))

	url, err := h.storage.Upload(reqCtx(c), key, f, fh.Size, mimeType)
	if err != nil {
		return fmt.Errorf("images: storage upload: %w", err)
	}

	return c.JSON(fiber.Map{
		"status": "success",
		"data":   fiber.Map{"url": url},
	})
}
