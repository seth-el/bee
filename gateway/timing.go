package gateway

import (
	"fmt"
	"net/http"
	"time"
)

func setServerTiming(writer http.ResponseWriter, name string, started time.Time) {
	if name == "" || started.IsZero() {
		return
	}
	duration := float64(time.Since(started).Microseconds()) / 1000
	if duration < 0 {
		duration = 0
	}
	writer.Header().Add("Server-Timing", fmt.Sprintf("%s;dur=%.1f", name, duration))
}
