package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type providerSpy struct {
	server        *httptest.Server
	mu            sync.Mutex
	calls         int
	concurrent    int
	maxConcurrent int
	models        []string
	fail          bool
	responseModel string
}

func newProviderSpy(t *testing.T, responseModel string) *providerSpy {
	t.Helper()
	spy := &providerSpy{responseModel: responseModel}
	spy.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		spy.mu.Lock()
		spy.calls++
		spy.concurrent++
		if spy.concurrent > spy.maxConcurrent {
			spy.maxConcurrent = spy.concurrent
		}
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(request.Body).Decode(&payload)
		spy.models = append(spy.models, payload.Model)
		fail := spy.fail
		spy.mu.Unlock()
		defer func() {
			spy.mu.Lock()
			spy.concurrent--
			spy.mu.Unlock()
		}()
		if fail {
			http.Error(writer, `{"error":"spy failure"}`, http.StatusBadGateway)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, fmt.Sprintf("data: {\"model\":\"%s\",\"choices\":[{\"delta\":{\"content\":\"spy\"}}]}\n\ndata: [DONE]\n\n", responseModel))
	}))
	t.Cleanup(spy.server.Close)
	return spy
}

func (spy *providerSpy) count() int {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	return spy.calls
}

func providerSpyConfig(main, child, manual *providerSpy) Config {
	config := DefaultConfig()
	config.Profiles = []ProviderProfile{
		{Name: "main-spy", Role: ProviderRoleMain, Provider: ProviderConfig{Type: DefaultProviderType, BaseURL: main.server.URL, Model: "main-model", TimeoutSeconds: 2}},
		{Name: "child-spy", Role: ProviderRoleChild, Provider: ProviderConfig{Type: DefaultProviderType, BaseURL: child.server.URL, Model: "child-model", TimeoutSeconds: 2}},
		{Name: "cloud-spy", Role: ProviderRoleManual, Provider: ProviderConfig{Type: DefaultProviderType, BaseURL: manual.server.URL, Model: "cloud-model", TimeoutSeconds: 2}},
	}
	config.Routing = ProviderRoutingConfig{MainProfile: "main-spy", ChildProfile: "child-spy", ManualProfiles: []string{"cloud-spy"}, MaxChildren: 3}
	return config
}

func TestProviderAcceptanceRoutesMainChildAndManualProfile(t *testing.T) {
	main, child, manual := newProviderSpy(t, "main-response"), newProviderSpy(t, "child-response"), newProviderSpy(t, "cloud-response")
	config := providerSpyConfig(main, child, manual)
	report := RunProviderAcceptance(context.Background(), config, []string{"main-spy", "child-spy", "cloud-spy"}, ProviderAcceptanceOptions{AllowCloudProbe: true, Timeout: time.Second, Nonce: "spy-routing"})
	if !report.Passed || len(report.Results) != 3 {
		t.Fatalf("report = %#v", report)
	}
	if main.count() != 1 || child.count() != 1 || manual.count() != 1 {
		t.Fatalf("counts main=%d child=%d manual=%d", main.count(), child.count(), manual.count())
	}
	for _, result := range report.Results {
		if result.FallbackAttempts != 0 || result.ErrorClass != "" || !result.ResponseReceived {
			t.Fatalf("result = %#v", result)
		}
	}
}

func TestProviderAcceptanceSkipsManualCloudWithoutOptIn(t *testing.T) {
	main, child, manual := newProviderSpy(t, "main-response"), newProviderSpy(t, "child-response"), newProviderSpy(t, "cloud-response")
	report := RunProviderAcceptance(context.Background(), providerSpyConfig(main, child, manual), nil, ProviderAcceptanceOptions{Timeout: time.Second})
	if !report.Passed || manual.count() != 0 || main.count() != 1 || child.count() != 1 {
		t.Fatalf("report=%#v counts=%d/%d/%d", report, main.count(), child.count(), manual.count())
	}
	if len(report.SkippedProfiles) != 1 || report.SkippedProfiles[0] != "cloud-spy" {
		t.Fatalf("skipped=%#v", report.SkippedProfiles)
	}
}

func TestProviderAcceptanceHasNoAutomaticFallback(t *testing.T) {
	main, child, manual := newProviderSpy(t, "main-response"), newProviderSpy(t, "child-response"), newProviderSpy(t, "cloud-response")
	main.fail = true
	report := RunProviderAcceptance(context.Background(), providerSpyConfig(main, child, manual), []string{"main-spy"}, ProviderAcceptanceOptions{Timeout: time.Second})
	if report.Passed || main.count() != 1 || child.count() != 0 || manual.count() != 0 {
		t.Fatalf("report=%#v counts=%d/%d/%d", report, main.count(), child.count(), manual.count())
	}
	if report.FallbackAttempts != 0 || report.Results[0].ErrorClass != "http_error" {
		t.Fatalf("result=%#v", report.Results[0])
	}
}

func TestProviderAcceptanceRedactsSecretsAndBoundsProbe(t *testing.T) {
	spy := newProviderSpy(t, "response")
	config := providerSpyConfig(spy, spy, spy)
	report := RunProviderAcceptance(context.Background(), config, []string{"cloud-spy"}, ProviderAcceptanceOptions{Timeout: time.Second})
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Authorization") || strings.Contains(string(data), "secret") {
		t.Fatalf("report contains sensitive text: %s", data)
	}
	if spy.count() != 0 {
		t.Fatal("manual profile was probed without cloud opt-in")
	}
}

func TestProviderAcceptanceRejectsInvalidNonce(t *testing.T) {
	spy := newProviderSpy(t, "response")
	result := ProbeProviderProfile(context.Background(), providerSpyConfig(spy, spy, spy), "main-spy", ProviderAcceptanceOptions{Nonce: "bad\nnonce"})
	if result.ErrorClass != "invalid_nonce" || result.Attempted || spy.count() != 0 {
		t.Fatalf("result=%#v calls=%d", result, spy.count())
	}
}

func TestProviderAcceptanceErrorClassHandlesCancellation(t *testing.T) {
	if got := providerAcceptanceErrorClass(errors.New("provider request: context deadline exceeded"), context.DeadlineExceeded); got != "timeout" {
		t.Fatalf("error class=%q", got)
	}
}

func TestProviderAcceptanceSanitizesResponseModelPath(t *testing.T) {
	if got := safeProviderModel("/mnt/models/Gemma/model.gguf"); got != "model.gguf" {
		t.Fatalf("safe model = %q", got)
	}
}
