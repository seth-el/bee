package bee

import (
	"context"
	"strings"
	"testing"
)

func TestEvidenceExtractorStrictValidationAndExactAnchors(t *testing.T) {
	provider := &specialistTestProvider{content: `{"items":[{"claim":"service responded","anchor":"service responded","category":"fact","confidence":0.9},{"claim":"response was healthy","anchor":"exit code: 0","category":"inference","confidence":0.7}],"uncertainties":["health is inferred"],"errors":[],"suggested_verification_targets":["check the endpoint again"]}`}
	config := DefaultSpecialistConfig()
	config.EvidenceEnabled = true
	service, err := newConfiguredChildSpecialistService(provider, "local", "test-child", "http://127.0.0.1:8082", config)
	if err != nil {
		t.Fatal(err)
	}
	extractor, err := NewEvidenceExtractor(service)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := extractor.Extract(context.Background(), "service responded\nexit code: 0")
	if err != nil {
		t.Fatal(err)
	}
	if len(packet.Items) != 2 || packet.Items[1].Anchor != "exit code: 0" {
		t.Fatalf("packet = %#v", packet)
	}
	if strings.Contains(packet.Items[1].Claim, "authoritative") {
		t.Fatal("authoritative language leaked")
	}
}

func TestEvidenceExtractorRejectsUnknownFieldsAndUntrustedBounds(t *testing.T) {
	config := DefaultSpecialistConfig()
	config.EvidenceEnabled = true
	config.EvidenceMaxInputBytes = 32
	service, err := newConfiguredChildSpecialistService(&specialistTestProvider{content: `{"anchors":["not present"],"unexpected":true}`}, "local", "test-child", "http://127.0.0.1:8082", config)
	if err != nil {
		t.Fatal(err)
	}
	extractor, err := NewEvidenceExtractor(service)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extractor.Extract(context.Background(), "bounded text"); err == nil {
		t.Fatal("invalid evidence unexpectedly accepted")
	}
	if _, err := extractor.Extract(context.Background(), strings.Repeat("x", 128)); err == nil {
		t.Fatal("invalid bounded evidence unexpectedly accepted")
	}
}
