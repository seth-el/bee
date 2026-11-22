package bee

import (
	"strings"
	"testing"
)

func TestWebConfigEffectiveTimeZone(t *testing.T) {
	if got := (WebConfig{}).EffectiveTimeZone(); got != DefaultWebTimeZone {
		t.Fatalf("blank time zone = %q, want %q", got, DefaultWebTimeZone)
	}
	config := WebConfig{TimeZone: " America/Los_Angeles "}
	if got := config.EffectiveTimeZone(); got != "America/Los_Angeles" {
		t.Fatalf("custom time zone = %q", got)
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("custom time zone validation: %v", err)
	}
}

func TestWebConfigRejectsInvalidTimeZone(t *testing.T) {
	tests := []struct {
		name string
		zone string
	}{
		{name: "unknown", zone: "Mars/Olympus"},
		{name: "control", zone: "America/New_York\n<script>"},
		{name: "too long", zone: strings.Repeat("A", 129)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := (WebConfig{TimeZone: test.zone}).Validate(); err == nil {
				t.Fatalf("Validate(%q) unexpectedly succeeded", test.zone)
			}
		})
	}
}
