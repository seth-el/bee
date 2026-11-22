package telegram

import "errors"

func retryableTelegram(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Temporary()
}
