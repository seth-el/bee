package bee

import (
	"testing"
	"time"
)

func TestBrowserWebConfigValidatesEndpointTransportAndPrivateControlPlane(t *testing.T) {
	if err := (BrowserWebConfig{Enabled: true, CamofoxURL: "http://203.0.113.200:9377", TimeoutSeconds: 2}).Validate(); err == nil {
		t.Fatal("non-loopback no-key endpoint unexpectedly accepted")
	}
	if err := (BrowserWebConfig{Enabled: true, CamofoxURL: "not-a-url"}).Validate(); err == nil {
		t.Fatal("invalid endpoint unexpectedly accepted")
	}
	if err := (BrowserWebConfig{Enabled: true, CamofoxURL: "http://127.0.0.1:9377", TimeoutSeconds: -1}).Validate(); err == nil {
		t.Fatal("invalid timeout unexpectedly accepted")
	}
	if err := (WebConfig{Enabled: false, Browser: BrowserWebConfig{Enabled: true, CamofoxURL: "http://127.0.0.1:9377"}}).Validate(); err == nil {
		t.Fatal("browser unexpectedly enabled without web")
	}
}

func TestWebConfigValidatesAvatarFilePaths(t *testing.T) {
	for _, path := range []string{"/tmp/user.png", "relative/bee.webp"} {
		if err := (WebConfig{UserAvatarFile: path, BeeAvatarFile: path}).Validate(); err != nil {
			t.Fatalf("avatar file path %q rejected: %v", path, err)
		}
	}
	for _, path := range []string{"avatar\x00.png", "avatar\n.png"} {
		if err := (WebConfig{UserAvatarFile: path}).Validate(); err == nil {
			t.Fatalf("unsafe avatar file path %q unexpectedly accepted", path)
		}
	}
}

func TestNewWebClientBuildsThinCamofoxBackend(t *testing.T) {
	client, err := NewWebClient(WebConfig{
		Enabled: true,
		Browser: BrowserWebConfig{
			Enabled:        true,
			CamofoxURL:     "http://127.0.0.1:9377",
			TimeoutSeconds: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	browser, ok := client.Browser.(*BrowserCamofoxClient)
	if !ok {
		t.Fatalf("browser type=%T", client.Browser)
	}
	if browser.HTTPClient == client.HTTPClient || browser.HTTPClient.Timeout != 2*time.Second {
		t.Fatalf("browser client=%p web client=%p browser timeout=%s", browser.HTTPClient, client.HTTPClient, browser.HTTPClient.Timeout)
	}
}
