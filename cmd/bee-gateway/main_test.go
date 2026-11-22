package main

import (
	"strings"
	"testing"
)

func TestGatewayCLIHasNoTelegramFlags(t *testing.T) {
	err := run([]string{"--telegram-config", "telegram.json"})
	if err == nil {
		t.Fatal("gateway unexpectedly accepted a Telegram flag")
	}
	if !strings.Contains(err.Error(), "flag provided but not defined: -telegram-config") {
		t.Fatalf("unexpected error = %v", err)
	}
}
