package handlers

import (
	"errors"
	"fmt"
	"strings"

	"github.com/go-playground/validator/v10"
)

var validate = validator.New()

// validationError converts validator.ValidationErrors into a human-readable
// string suitable for returning as a 400 response body.
func validationError(err error) error {
	ve, ok := errors.AsType[validator.ValidationErrors](err)
	if !ok {
		return err
	}

	msgs := make([]string, 0, len(ve))
	for _, fe := range ve {
		msgs = append(msgs, fieldError(fe))
	}
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}

func fieldError(fe validator.FieldError) string {
	field := strings.ToLower(fe.Field())
	switch fe.Tag() {
	case "required":
		return field + " is required"
	case "email":
		return field + " must be a valid email address"
	default:
		return field + " is invalid"
	}
}
