package bee

import (
	"errors"
	"strings"
	"unicode"
)

func ValidateModelName(model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("model name is required")
	}
	if len(model) > 128 {
		return errors.New("model name exceeds 128 bytes")
	}
	for _, r := range model {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("model name contains whitespace or control characters")
		}
	}
	return nil
}
